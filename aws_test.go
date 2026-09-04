package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
	"github.com/google/go-cmp/cmp"
	"github.com/upbound/function-aws-query/input/v1beta1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
)

// respStub is an aws.HTTPClient that returns canned responses in sequence (the
// last one repeats), so the real AWS SDK marshals the request and unmarshals our
// response — exercising the handlers' projection and pagination for real.
type respStub struct {
	bodies      []string
	contentType string
	calls       int
	// requests records each marshalled request body, so a test can assert what
	// was actually sent (e.g. that a filter went server-side).
	requests []string
}

func (s *respStub) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		sent, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		s.requests = append(s.requests, string(sent))
	}
	body := s.bodies[s.calls]
	if s.calls < len(s.bodies)-1 {
		s.calls++
	}
	h := http.Header{}
	if s.contentType != "" {
		h.Set("Content-Type", s.contentType)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func stubCfg(rt *respStub) aws.Config {
	return aws.Config{
		Region:           "eu-central-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       rt,
		RetryMaxAttempts: 1,
	}
}

func newQuery() *AWSQuery { return &AWSQuery{log: logging.NewNopLogger()} }

// --- pure helpers -----------------------------------------------------------

func TestToEC2Filters(t *testing.T) {
	got := toEC2Filters([]v1beta1.Filter{{Name: "state", Values: []string{"available"}}})
	if len(got) != 1 || aws.ToString(got[0].Name) != "state" || len(got[0].Values) != 1 || got[0].Values[0] != "available" {
		t.Errorf("toEC2Filters wrong result: %+v", got)
	}
	if toEC2Filters(nil) != nil {
		t.Error("toEC2Filters(nil) should be nil")
	}
}

func TestToTagFilters(t *testing.T) {
	got := toTagFilters([]v1beta1.Filter{{Name: "Environment", Values: []string{"prod"}}})
	if len(got) != 1 || aws.ToString(got[0].Key) != "Environment" || got[0].Values[0] != "prod" {
		t.Errorf("toTagFilters wrong result: %+v", got)
	}
	if toTagFilters(nil) != nil {
		t.Error("toTagFilters(nil) should be nil")
	}
}

func TestToSTSTags(t *testing.T) {
	got := toSTSTags(map[string]string{"a": "1", "b": "2"})
	if len(got) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(got))
	}
	seen := map[string]string{}
	for _, tg := range got {
		seen[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	if seen["a"] != "1" || seen["b"] != "2" {
		t.Errorf("toSTSTags wrong result: %+v", seen)
	}
	if toSTSTags(nil) != nil {
		t.Error("toSTSTags(nil) should be nil")
	}
}

func TestQuotaToMap(t *testing.T) {
	got := quotaToMap(sqtypes.ServiceQuota{
		QuotaCode: aws.String("L-1"), QuotaName: aws.String("VPCs"),
		Unit: aws.String("None"), Adjustable: true, GlobalQuota: false, Value: aws.Float64(5),
	})
	want := map[string]any{
		"quotaCode": "L-1", "quotaName": "VPCs", "unit": "None",
		"adjustable": true, "globalQuota": false, "value": float64(5),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("quotaToMap: -want +got:\n%s", diff)
	}
	// Value omitted when nil.
	if _, ok := quotaToMap(sqtypes.ServiceQuota{QuotaCode: aws.String("L-2")})["value"]; ok {
		t.Error("value should be omitted when nil")
	}
}

func TestIsGlobalQuery(t *testing.T) {
	for _, q := range []string{"GetCallerIdentity", "DescribeRegions"} {
		if !isGlobalQuery(q) {
			t.Errorf("%s should be global", q)
		}
	}
	if isGlobalQuery("DescribeVpcs") {
		t.Error("DescribeVpcs should not be global")
	}
}

func TestIniRegion(t *testing.T) {
	if got := iniRegion([]byte("[default]\nregion = eu-west-1\n")); got != "eu-west-1" {
		t.Errorf("iniRegion = %q, want eu-west-1", got)
	}
	if got := iniRegion(nil); got != "" {
		t.Errorf("iniRegion(nil) = %q, want empty", got)
	}
}

func TestResolveRegion(t *testing.T) {
	creds := map[string][]byte{"credentials": []byte("[default]\nregion = eu-west-1\n")}
	cases := map[string]struct {
		creds map[string][]byte
		in    *v1beta1.Input
		want  string
	}{
		"InputRegionWins": {creds: creds, in: &v1beta1.Input{Region: aws.String("ap-south-1"), QueryType: "DescribeVpcs"}, want: "ap-south-1"},
		"INIRegion":       {creds: creds, in: &v1beta1.Input{QueryType: "DescribeVpcs"}, want: "eu-west-1"},
		"GlobalFallback":  {creds: map[string][]byte{}, in: &v1beta1.Input{QueryType: "DescribeRegions"}, want: "us-east-1"},
		"EmptyNonGlobal":  {creds: map[string][]byte{}, in: &v1beta1.Input{QueryType: "DescribeVpcs"}, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := resolveRegion(tc.creds, tc.in); got != tc.want {
				t.Errorf("resolveRegion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWebIdentityTokenRetriever(t *testing.T) {
	t.Run("SecretEmpty", func(t *testing.T) {
		if _, err := webIdentityTokenRetriever(nil, &v1beta1.WebIdentity{RoleARN: "r"}); err == nil {
			t.Error("expected error for empty token secret")
		}
	})
	t.Run("SecretOK", func(t *testing.T) {
		r, err := webIdentityTokenRetriever([]byte("jwt"), &v1beta1.WebIdentity{RoleARN: "r"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		tok, _ := r.GetIdentityToken()
		if string(tok) != "jwt" {
			t.Errorf("token = %q, want jwt", tok)
		}
	})
	t.Run("FilesystemNoPath", func(t *testing.T) {
		if _, err := webIdentityTokenRetriever(nil, &v1beta1.WebIdentity{RoleARN: "r", TokenConfig: &v1beta1.TokenConfig{Source: "Filesystem"}}); err == nil {
			t.Error("expected error for missing fsPath")
		}
	})
	t.Run("FilesystemOK", func(t *testing.T) {
		if _, err := webIdentityTokenRetriever(nil, &v1beta1.WebIdentity{RoleARN: "r", TokenConfig: &v1beta1.TokenConfig{Source: "Filesystem", FSPath: aws.String("/tmp/token")}}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("Unsupported", func(t *testing.T) {
		if _, err := webIdentityTokenRetriever(nil, &v1beta1.WebIdentity{RoleARN: "r", TokenConfig: &v1beta1.TokenConfig{Source: "Bogus"}}); err == nil {
			t.Error("expected error for unsupported source")
		}
	})
}

// --- buildAWSConfig ---------------------------------------------------------

func TestBuildAWSConfig(t *testing.T) {
	iniCreds := map[string][]byte{"credentials": []byte("[default]\naws_access_key_id = AK\naws_secret_access_key = SK\nregion = eu-west-1\n")}

	t.Run("SecretResolvesCredsAndRegion", func(t *testing.T) {
		cfg, err := buildAWSConfig(context.Background(), iniCreds, &v1beta1.Input{QueryType: "DescribeVpcs"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", cfg.Region)
		}
		got, err := cfg.Credentials.Retrieve(context.Background())
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		if got.AccessKeyID != "AK" || got.SecretAccessKey != "SK" {
			t.Errorf("creds = %+v, want AK/SK", got)
		}
	})

	t.Run("SecretEmptyErrors", func(t *testing.T) {
		if _, err := buildAWSConfig(context.Background(), map[string][]byte{}, &v1beta1.Input{QueryType: "DescribeVpcs"}); err == nil {
			t.Error("expected error for missing credentials")
		}
	})

	t.Run("WebIdentityRequiresRoleARN", func(t *testing.T) {
		in := &v1beta1.Input{QueryType: "DescribeRegions", Identity: &v1beta1.Identity{Source: v1beta1.IdentitySourceWebIdentity}}
		if _, err := buildAWSConfig(context.Background(), map[string][]byte{}, in); err == nil {
			t.Error("expected error for missing roleARN")
		}
	})

	t.Run("UnsupportedSource", func(t *testing.T) {
		in := &v1beta1.Input{QueryType: "DescribeRegions", Identity: &v1beta1.Identity{Source: "Bogus"}}
		if _, err := buildAWSConfig(context.Background(), map[string][]byte{}, in); err == nil {
			t.Error("expected error for unsupported source")
		}
	})

	t.Run("IRSANoSecret", func(t *testing.T) {
		in := &v1beta1.Input{QueryType: "DescribeRegions", Identity: &v1beta1.Identity{Source: v1beta1.IdentitySourceIRSA}}
		cfg, err := buildAWSConfig(context.Background(), map[string][]byte{}, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Region != "us-east-1" {
			t.Errorf("region = %q, want us-east-1 (global fallback)", cfg.Region)
		}
	})

	t.Run("AssumeRoleChainWires", func(t *testing.T) {
		in := &v1beta1.Input{QueryType: "DescribeVpcs", Identity: &v1beta1.Identity{
			Source:          v1beta1.IdentitySourceSecret,
			AssumeRoleChain: []v1beta1.AssumeRole{{RoleARN: "arn:aws:iam::222:role/x"}},
		}}
		cfg, err := buildAWSConfig(context.Background(), iniCreds, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Credentials == nil {
			t.Error("expected credentials provider to be set by the chain")
		}
	})
}

// --- resolveRegionRef -------------------------------------------------------

func TestResolveRegionRef(t *testing.T) {
	f := &Function{log: logging.NewNopLogger()}

	t.Run("Spec", func(t *testing.T) {
		req := &fnv1.RunFunctionRequest{
			Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(
				`{"apiVersion":"example.io/v1alpha1","kind":"XAccount","metadata":{"name":"x"},"spec":{"region":"ap-south-1"}}`)}},
		}
		in := &v1beta1.Input{RegionRef: aws.String("spec.region")}
		if err := f.resolveRegionRef(req, in, &fnv1.RunFunctionResponse{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if aws.ToString(in.Region) != "ap-south-1" {
			t.Errorf("region = %q, want ap-south-1", aws.ToString(in.Region))
		}
	})

	t.Run("Status", func(t *testing.T) {
		req := &fnv1.RunFunctionRequest{
			Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(
				`{"apiVersion":"example.io/v1alpha1","kind":"XAccount","metadata":{"name":"x"},"status":{"chosenRegion":"sa-east-1"}}`)}},
		}
		in := &v1beta1.Input{RegionRef: aws.String("status.chosenRegion")}
		if err := f.resolveRegionRef(req, in, &fnv1.RunFunctionResponse{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if aws.ToString(in.Region) != "sa-east-1" {
			t.Errorf("region = %q, want sa-east-1", aws.ToString(in.Region))
		}
	})

	t.Run("Unrecognized", func(t *testing.T) {
		in := &v1beta1.Input{RegionRef: aws.String("bogus.x")}
		if err := f.resolveRegionRef(&fnv1.RunFunctionRequest{}, in, &fnv1.RunFunctionResponse{}); err == nil {
			t.Error("expected error for unrecognized regionRef prefix")
		}
	})

	t.Run("NoRef", func(t *testing.T) {
		in := &v1beta1.Input{}
		if err := f.resolveRegionRef(&fnv1.RunFunctionRequest{}, in, &fnv1.RunFunctionResponse{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if in.Region != nil {
			t.Error("region should remain unset")
		}
	})
}

// --- handler validation guards (no network) ---------------------------------

func TestHandlerValidationGuards(t *testing.T) {
	q := newQuery()
	ctx := context.Background()
	noRegion := aws.Config{} // empty region triggers the region-required guard

	cases := map[string]func() (any, error){
		"AZsNoRegion":       func() (any, error) { return q.describeAvailabilityZones(ctx, noRegion, &v1beta1.Input{}) },
		"ImagesNoRegion":    func() (any, error) { return q.describeImages(ctx, noRegion, &v1beta1.Input{}) },
		"QuotasNoRegion":    func() (any, error) { return q.listServiceQuotas(ctx, noRegion, &v1beta1.Input{}) },
		"GetQuotaNoRegion":  func() (any, error) { return q.getServiceQuota(ctx, noRegion, &v1beta1.Input{}) },
		"ListResNoRegion":   func() (any, error) { return q.listResources(ctx, noRegion, &v1beta1.Input{}) },
		"GetResNoRegion":    func() (any, error) { return q.getResources(ctx, noRegion, &v1beta1.Input{}) },
		"ImagesNoFilter":    func() (any, error) { return q.describeImages(ctx, stubCfg(&respStub{}), &v1beta1.Input{}) },
		"QuotasNoService":   func() (any, error) { return q.listServiceQuotas(ctx, stubCfg(&respStub{}), &v1beta1.Input{}) },
		"GetQuotaNoCodes":   func() (any, error) { return q.getServiceQuota(ctx, stubCfg(&respStub{}), &v1beta1.Input{}) },
		"ListResNoTypeName": func() (any, error) { return q.listResources(ctx, stubCfg(&respStub{}), &v1beta1.Input{}) },
		"Ec2NoRegion":       func() (any, error) { return q.describeEc2(ctx, noRegion, &v1beta1.Input{}) },
		"Ec2NoOperation":    func() (any, error) { return q.describeEc2(ctx, stubCfg(&respStub{}), &v1beta1.Input{}) },
		"Ec2BadOperation": func() (any, error) {
			return q.describeEc2(ctx, stubCfg(&respStub{}), &v1beta1.Input{Parameters: map[string]string{"operation": "Vpcs"}})
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := call(); err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
}

// --- handlers with stubbed HTTP transport (projection + pagination) ---------

func TestGetCallerIdentity(t *testing.T) {
	body := `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">` +
		`<GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/test</Arn>` +
		`<UserId>AIDEXAMPLE</UserId><Account>123456789012</Account></GetCallerIdentityResult>` +
		`<ResponseMetadata><RequestId>req</RequestId></ResponseMetadata></GetCallerIdentityResponse>`
	got, err := newQuery().getCallerIdentity(context.Background(), stubCfg(&respStub{bodies: []string{body}, contentType: "text/xml"}), &v1beta1.Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"account": "123456789012", "arn": "arn:aws:iam::123456789012:user/test", "userId": "AIDEXAMPLE"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestDescribeRegions(t *testing.T) {
	body := `<DescribeRegionsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<regionInfo><item><regionName>us-east-1</regionName><regionEndpoint>ec2.us-east-1.amazonaws.com</regionEndpoint><optInStatus>opt-in-not-required</optInStatus></item></regionInfo>` +
		`</DescribeRegionsResponse>`
	got, err := newQuery().describeRegions(context.Background(), stubCfg(&respStub{bodies: []string{body}, contentType: "text/xml"}), &v1beta1.Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{map[string]any{"name": "us-east-1", "endpoint": "ec2.us-east-1.amazonaws.com", "optInStatus": "opt-in-not-required"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestListServiceQuotasPaginates(t *testing.T) {
	page1 := `{"NextToken":"n","Quotas":[{"QuotaCode":"L-1","QuotaName":"VPCs","Value":5.0,"Unit":"None","Adjustable":true,"GlobalQuota":false}]}`
	page2 := `{"Quotas":[{"QuotaCode":"L-2","QuotaName":"EIPs","Value":10.0,"Unit":"None","Adjustable":false,"GlobalQuota":false}]}`
	in := &v1beta1.Input{Parameters: map[string]string{"serviceCode": "ec2"}}
	got, err := newQuery().listServiceQuotas(context.Background(), stubCfg(&respStub{bodies: []string{page1, page2}, contentType: "application/x-amz-json-1.1"}), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 quotas across 2 pages, got %#v", got)
	}
}

func TestGetResourcesPaginates(t *testing.T) {
	page1 := `{"PaginationToken":"tok","ResourceTagMappingList":[{"ResourceARN":"arn:a","Tags":[{"Key":"Environment","Value":"prod"}]}]}`
	page2 := `{"PaginationToken":"","ResourceTagMappingList":[{"ResourceARN":"arn:b","Tags":[]}]}`
	in := &v1beta1.Input{Parameters: map[string]string{"resourceTypeFilters": "ec2:subnet"}}
	got, err := newQuery().getResources(context.Background(), stubCfg(&respStub{bodies: []string{page1, page2}, contentType: "application/x-amz-json-1.1"}), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{
		map[string]any{"arn": "arn:a", "tags": map[string]any{"Environment": "prod"}},
		map[string]any{"arn": "arn:b", "tags": map[string]any{}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

// TestListResourcesFiltersClientSide uses hydrate=false so the client-side
// filter runs against the list properties directly (no GetResource calls).
func TestListResourcesFiltersClientSide(t *testing.T) {
	body := `{"TypeName":"AWS::EC2::VPC","ResourceDescriptions":[` +
		`{"Identifier":"vpc-1","Properties":"{\"VpcId\":\"vpc-1\",\"Tags\":[{\"Key\":\"Environment\",\"Value\":\"prod\"}]}"},` +
		`{"Identifier":"vpc-2","Properties":"{\"VpcId\":\"vpc-2\",\"Tags\":[{\"Key\":\"Environment\",\"Value\":\"dev\"}]}"}` +
		`]}`
	in := &v1beta1.Input{
		Parameters: map[string]string{"typeName": "AWS::EC2::VPC", "hydrate": "false"},
		Filters:    []v1beta1.Filter{{Name: "tag:Environment", Values: []string{"prod"}}},
	}
	got, err := newQuery().listResources(context.Background(), stubCfg(&respStub{bodies: []string{body}, contentType: "application/x-amz-json-1.0"}), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("expected 1 VPC after client-side tag filter, got %#v", got)
	}
	m := list[0].(map[string]any)
	if m["identifier"] != "vpc-1" {
		t.Errorf("identifier = %v, want vpc-1", m["identifier"])
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || props["VpcId"] != "vpc-1" {
		t.Errorf("properties = %#v, want VpcId vpc-1", m["properties"])
	}
}

// TestListResourcesHydrates covers the default path: ListResources returns only
// the identifier, then GetResource hydrates the full model (incl. Tags), which
// the client-side filter then matches.
func TestListResourcesHydrates(t *testing.T) {
	listBody := `{"TypeName":"AWS::EC2::VPC","ResourceDescriptions":[{"Identifier":"vpc-1","Properties":"{\"VpcId\":\"vpc-1\"}"}]}`
	getBody := `{"TypeName":"AWS::EC2::VPC","ResourceDescription":{"Identifier":"vpc-1","Properties":"{\"VpcId\":\"vpc-1\",\"CidrBlock\":\"10.0.0.0/24\",\"Tags\":[{\"Key\":\"Environment\",\"Value\":\"prod\"}]}"}}`
	in := &v1beta1.Input{
		Parameters: map[string]string{"typeName": "AWS::EC2::VPC"}, // hydrate defaults to true
		Filters:    []v1beta1.Filter{{Name: "tag:Environment", Values: []string{"prod"}}},
	}
	// First HTTP call = ListResources, subsequent = GetResource (last repeats).
	stub := &respStub{bodies: []string{listBody, getBody}, contentType: "application/x-amz-json-1.0"}
	got, err := newQuery().listResources(context.Background(), stubCfg(stub), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("expected 1 hydrated VPC, got %#v", got)
	}
	props := list[0].(map[string]any)["properties"].(map[string]any)
	if props["CidrBlock"] != "10.0.0.0/24" {
		t.Errorf("expected hydrated CidrBlock, got %#v", props)
	}
}

func TestDescribeImages(t *testing.T) {
	body := `<DescribeImagesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<imagesSet><item><imageId>ami-1</imageId><name>ubuntu</name><imageOwnerId>099720109477</imageOwnerId>` +
		`<creationDate>2024-01-01T00:00:00.000Z</creationDate><architecture>x86_64</architecture>` +
		`<imageState>available</imageState><rootDeviceType>ebs</rootDeviceType><description>desc</description></item></imagesSet>` +
		`</DescribeImagesResponse>`
	in := &v1beta1.Input{Parameters: map[string]string{"owners": "099720109477"}}
	got, err := newQuery().describeImages(context.Background(), stubCfg(&respStub{bodies: []string{body}, contentType: "text/xml"}), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{map[string]any{
		"imageId": "ami-1", "name": "ubuntu", "ownerId": "099720109477",
		"creationDate": "2024-01-01T00:00:00.000Z", "architecture": "x86_64",
		"state": "available", "rootDeviceType": "ebs", "description": "desc",
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

// --- DescribeEc2 ------------------------------------------------------------

const (
	routeTablesPage1 = `<DescribeRouteTablesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<routeTableSet><item><routeTableId>rtb-1</routeTableId><vpcId>vpc-1</vpcId><ownerId>123456789012</ownerId>` +
		`<associationSet><item><routeTableAssociationId>rtbassoc-main</routeTableAssociationId><routeTableId>rtb-1</routeTableId>` +
		`<main>true</main><associationState><state>associated</state></associationState></item></associationSet>` +
		`<routeSet><item><destinationCidrBlock>10.0.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId>` +
		`<origin>CreateRouteTable</origin><state>active</state></item></routeSet>` +
		`<tagSet><item><key>Name</key><value>main</value></item></tagSet></item></routeTableSet>` +
		`<nextToken>tok</nextToken></DescribeRouteTablesResponse>`

	routeTablesPage2 = `<DescribeRouteTablesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<routeTableSet><item><routeTableId>rtb-2</routeTableId><vpcId>vpc-1</vpcId><ownerId>123456789012</ownerId>` +
		`<associationSet><item><routeTableAssociationId>rtbassoc-2</routeTableAssociationId><routeTableId>rtb-2</routeTableId>` +
		`<subnetId>subnet-1</subnetId><main>false</main><associationState><state>associated</state></associationState>` +
		`</item></associationSet><routeSet/><tagSet/></item></routeTableSet></DescribeRouteTablesResponse>`

	subnetsBody = `<DescribeSubnetsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<subnetSet><item><subnetId>subnet-1</subnetId>` +
		`<subnetArn>arn:aws:ec2:eu-central-1:123456789012:subnet/subnet-1</subnetArn>` +
		`<vpcId>vpc-1</vpcId><ownerId>123456789012</ownerId><availabilityZone>eu-central-1a</availabilityZone>` +
		`<availabilityZoneId>euc1-az2</availabilityZoneId><cidrBlock>10.0.1.0/24</cidrBlock><state>available</state>` +
		`<defaultForAz>false</defaultForAz><mapPublicIpOnLaunch>true</mapPublicIpOnLaunch>` +
		`<availableIpAddressCount>250</availableIpAddressCount>` +
		`<tagSet><item><key>Name</key><value>public-a</value></item></tagSet></item></subnetSet></DescribeSubnetsResponse>`

	securityGroupRulesBody = `<DescribeSecurityGroupRulesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId>` +
		`<securityGroupRuleSet><item><securityGroupRuleId>sgr-1</securityGroupRuleId><groupId>sg-1</groupId>` +
		`<securityGroupRuleArn>arn:aws:ec2:eu-central-1:123456789012:security-group-rule/sgr-1</securityGroupRuleArn>` +
		`<groupOwnerId>123456789012</groupOwnerId><isEgress>false</isEgress><ipProtocol>tcp</ipProtocol>` +
		`<fromPort>443</fromPort><toPort>443</toPort><cidrIpv4>0.0.0.0/0</cidrIpv4><description>https</description>` +
		`<tagSet><item><key>Name</key><value>ingress</value></item></tagSet></item>` +
		`<item><securityGroupRuleId>sgr-2</securityGroupRuleId><groupId>sg-1</groupId><groupOwnerId>123456789012</groupOwnerId>` +
		`<isEgress>true</isEgress><ipProtocol>-1</ipProtocol><fromPort>-1</fromPort><toPort>-1</toPort>` +
		`<referencedGroupInfo><groupId>sg-2</groupId></referencedGroupInfo></item>` +
		`<item><securityGroupRuleId>sgr-3</securityGroupRuleId><groupId>sg-1</groupId><groupOwnerId>123456789012</groupOwnerId>` +
		`<isEgress>false</isEgress><ipProtocol>icmp</ipProtocol><prefixListId>pl-1</prefixListId>` +
		`</item></securityGroupRuleSet></DescribeSecurityGroupRulesResponse>`
)

func ec2Input(operation string) *v1beta1.Input {
	return &v1beta1.Input{
		Parameters: map[string]string{"operation": operation},
		Filters:    []v1beta1.Filter{{Name: "vpc-id", Values: []string{"vpc-1"}}},
	}
}

// TestDescribeEc2Dispatches proves every allow-listed operation reaches its own
// describe call, and that an unsupported one names the supported values.
func TestDescribeEc2Dispatches(t *testing.T) {
	if newQuery().registry()["DescribeEc2"] == nil {
		t.Fatal("DescribeEc2 is not wired into the handler registry")
	}

	cases := map[string]struct {
		operation string
		body      string
	}{
		"RouteTables":        {operation: "RouteTables", body: routeTablesPage2},
		"SecurityGroupRules": {operation: "SecurityGroupRules", body: securityGroupRulesBody},
		"Subnets":            {operation: "Subnets", body: subnetsBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := newQuery().describeEc2(context.Background(),
				stubCfg(&respStub{bodies: []string{tc.body}, contentType: "text/xml"}), ec2Input(tc.operation))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			list, ok := got.([]any)
			if !ok || len(list) == 0 {
				t.Fatalf("expected a non-empty list, got %#v", got)
			}
		})
	}

	t.Run("Unsupported", func(t *testing.T) {
		_, err := newQuery().describeEc2(context.Background(), stubCfg(&respStub{}), ec2Input("Vpcs"))
		if err == nil {
			t.Fatal("expected an error for an unsupported operation")
		}
		for _, want := range []string{"RouteTables", "SecurityGroupRules", "Subnets"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name %s: %v", want, err)
			}
		}
	})
}

// TestDescribeEc2RouteTablesPaginates covers the projection (associations, incl.
// the main association ID) across two pages.
func TestDescribeEc2RouteTablesPaginates(t *testing.T) {
	stub := &respStub{bodies: []string{routeTablesPage1, routeTablesPage2}, contentType: "text/xml"}
	got, err := newQuery().describeEc2(context.Background(), stubCfg(stub), ec2Input("RouteTables"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{
		map[string]any{
			"routeTableId": "rtb-1", "vpcId": "vpc-1", "ownerId": "123456789012",
			"associations": []any{map[string]any{
				"routeTableAssociationId": "rtbassoc-main", "routeTableId": "rtb-1",
				"subnetId": "", "gatewayId": "", "main": true, "state": "associated",
			}},
			"routes": []any{map[string]any{
				"destinationCidrBlock": "10.0.0.0/16", "destinationIpv6CidrBlock": "",
				"destinationPrefixListId": "", "carrierGatewayId": "", "coreNetworkArn": "",
				"egressOnlyInternetGatewayId": "", "gatewayId": "local", "instanceId": "",
				"localGatewayId": "", "natGatewayId": "", "networkInterfaceId": "",
				"transitGatewayId": "", "vpcPeeringConnectionId": "",
				"origin": "CreateRouteTable", "state": "active",
			}},
			"tags": map[string]any{"Name": "main"},
		},
		map[string]any{
			"routeTableId": "rtb-2", "vpcId": "vpc-1", "ownerId": "123456789012",
			"associations": []any{map[string]any{
				"routeTableAssociationId": "rtbassoc-2", "routeTableId": "rtb-2",
				"subnetId": "subnet-1", "gatewayId": "", "main": false, "state": "associated",
			}},
			"routes": []any{},
			"tags":   map[string]any{},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
	if len(stub.requests) != 2 {
		t.Fatalf("expected 2 requests (one per page), got %d", len(stub.requests))
	}
	// vpc-id must go server-side - that is the whole point over ListResources.
	if !strings.Contains(stub.requests[0], "Filter.1.Name=vpc-id") {
		t.Errorf("vpc-id filter not sent server-side: %s", stub.requests[0])
	}
}

// Isolates the region guard: the shared guards table only asserts err != nil,
// which the SDK's endpoint-resolution error satisfies on its own.
func TestDescribeEc2RegionGuard(t *testing.T) {
	in := &v1beta1.Input{Parameters: map[string]string{"operation": "Subnets"}}
	_, err := newQuery().describeEc2(context.Background(), aws.Config{}, in)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "requires a region") {
		t.Errorf("expected the region guard, got: %v", err)
	}
}

func TestDescribeEc2Subnets(t *testing.T) {
	stub := &respStub{bodies: []string{subnetsBody}, contentType: "text/xml"}
	got, err := newQuery().describeEc2(context.Background(), stubCfg(stub), ec2Input("Subnets"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{map[string]any{
		"subnetId": "subnet-1", "subnetArn": "arn:aws:ec2:eu-central-1:123456789012:subnet/subnet-1",
		"vpcId": "vpc-1", "ownerId": "123456789012", "availabilityZone": "eu-central-1a",
		"availabilityZoneId": "euc1-az2", "cidrBlock": "10.0.1.0/24", "state": "available",
		"defaultForAz": false, "mapPublicIpOnLaunch": true, "availableIpAddressCount": int64(250),
		"tags": map[string]any{"Name": "public-a"},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
	if !strings.Contains(stub.requests[0], "Filter.1.Name=vpc-id") {
		t.Errorf("filter not sent server-side: %s", stub.requests[0])
	}
}

// Optional keys: referencedGroupId only for group references, ports only when
// on the wire - a live all-protocol rule reports -1/-1, not nothing.
func TestDescribeEc2SecurityGroupRules(t *testing.T) {
	stub := &respStub{bodies: []string{securityGroupRulesBody}, contentType: "text/xml"}
	got, err := newQuery().describeEc2(context.Background(), stubCfg(stub), ec2Input("SecurityGroupRules"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{
		map[string]any{
			"securityGroupRuleId":  "sgr-1",
			"securityGroupRuleArn": "arn:aws:ec2:eu-central-1:123456789012:security-group-rule/sgr-1",
			"groupId":              "sg-1", "groupOwnerId": "123456789012",
			"isEgress": false, "ipProtocol": "tcp", "fromPort": int64(443), "toPort": int64(443),
			"cidrIpv4": "0.0.0.0/0", "cidrIpv6": "", "prefixListId": "", "description": "https",
			"tags": map[string]any{"Name": "ingress"},
		},
		map[string]any{
			"securityGroupRuleId": "sgr-2", "securityGroupRuleArn": "",
			"groupId": "sg-1", "groupOwnerId": "123456789012",
			"isEgress": true, "ipProtocol": "-1", "fromPort": int64(-1), "toPort": int64(-1),
			"cidrIpv4": "", "cidrIpv6": "",
			"prefixListId": "", "description": "", "referencedGroupId": "sg-2",
			"tags": map[string]any{},
		},
		map[string]any{
			"securityGroupRuleId": "sgr-3", "securityGroupRuleArn": "",
			"groupId": "sg-1", "groupOwnerId": "123456789012",
			"isEgress": false, "ipProtocol": "icmp", "cidrIpv4": "", "cidrIpv6": "",
			"prefixListId": "pl-1", "description": "", "tags": map[string]any{},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
	if !strings.Contains(stub.requests[0], "Filter.1.Name=vpc-id") {
		t.Errorf("filter not sent server-side: %s", stub.requests[0])
	}
}

func TestEc2TagsToMap(t *testing.T) {
	got := ec2TagsToMap([]ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("x")}})
	if diff := cmp.Diff(map[string]any{"Name": "x"}, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{}, ec2TagsToMap(nil)); diff != "" {
		t.Errorf("ec2TagsToMap(nil) should be an empty map:\n%s", diff)
	}
}

func TestPutInt32(t *testing.T) {
	m := map[string]any{}
	putInt32(m, "set", aws.Int32(7))
	putInt32(m, "unset", nil)
	if diff := cmp.Diff(map[string]any{"set": int64(7)}, m); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

// --- dispatch + remaining skip paths ----------------------------------------

func TestAWSQueryUnsupportedType(t *testing.T) {
	creds := map[string][]byte{"credentials": []byte("[default]\naws_access_key_id = AK\naws_secret_access_key = SK\n")}
	if _, err := newQuery().awsQuery(context.Background(), creds, &v1beta1.Input{QueryType: "Nope"}); err == nil {
		t.Error("expected an error for an unsupported queryType")
	}
}

func TestRunFunctionSkipContextTarget(t *testing.T) {
	called := false
	f := &Function{log: logging.NewNopLogger(), awsQuery: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
		called = true
		return nil, nil
	}}}
	ctx, err := structpb.NewStruct(map[string]any{"existing": "data"})
	if err != nil {
		t.Fatal(err)
	}
	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test"},
		Input: resource.MustStructJSON(`{
			"apiVersion":"aws.fn.crossplane.io/v1beta1","kind":"Input",
			"queryType":"DescribeRegions","target":"context.existing","skipQueryWhenTargetHasData":true
		}`),
		Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observedXR)}},
		Context:  ctx,
	}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Error("awsQuery should not be called when the context target already has data")
	}
	if !hasCondition(rsp, "FunctionSkip") {
		t.Errorf("expected a FunctionSkip condition, got: %v", rsp.GetConditions())
	}
}

func TestToFilters(t *testing.T) {
	t.Run("OK", func(t *testing.T) {
		v := []any{
			map[string]any{"name": "tag:Env", "values": []any{"prod", "dev"}},
			map[string]any{"name": "state", "values": []any{"available"}},
		}
		got, err := toFilters(v)
		if err != nil {
			t.Fatal(err)
		}
		want := []v1beta1.Filter{
			{Name: "tag:Env", Values: []string{"prod", "dev"}},
			{Name: "state", Values: []string{"available"}},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("-want +got:\n%s", diff)
		}
	})
	t.Run("NotAList", func(t *testing.T) {
		if _, err := toFilters(map[string]any{}); err == nil {
			t.Error("expected error for non-list")
		}
	})
	t.Run("MissingName", func(t *testing.T) {
		if _, err := toFilters([]any{map[string]any{"values": []any{"x"}}}); err == nil {
			t.Error("expected error for missing name")
		}
	})
	t.Run("StringifiesValues", func(t *testing.T) {
		got, err := toFilters([]any{map[string]any{"name": "n", "values": []any{float64(1), true}}})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Values[0] != "1" || got[0].Values[1] != "true" {
			t.Errorf("stringify failed: %v", got[0].Values)
		}
	})
}

func TestToParameters(t *testing.T) {
	t.Run("OK", func(t *testing.T) {
		got, err := toParameters(map[string]any{"serviceCode": "ec2", "n": float64(5)})
		if err != nil {
			t.Fatal(err)
		}
		if got["serviceCode"] != "ec2" || got["n"] != "5" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("NotAMap", func(t *testing.T) {
		if _, err := toParameters([]any{}); err == nil {
			t.Error("expected error for non-map")
		}
	})
}

// TestRunFunctionResolvesRefs proves region/filters/parameters are resolved from
// the XR spec (overriding static values) before the query runs.
func TestRunFunctionResolvesRefs(t *testing.T) {
	var seen *v1beta1.Input
	f := &Function{log: logging.NewNopLogger(), awsQuery: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, in *v1beta1.Input) (any, error) {
		seen = in
		return []any{}, nil
	}}}
	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test"},
		Input: resource.MustStructJSON(`{
			"apiVersion":"aws.fn.crossplane.io/v1beta1","kind":"Input",
			"queryType":"DescribeImages",
			"regionRef":"spec.region",
			"parameters":{"stale":"yes"},
			"parametersRef":"spec.imageParams",
			"filters":[{"name":"stale","values":["x"]}],
			"filtersRef":"spec.imageFilters",
			"target":"status.amis"
		}`),
		Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(`{
			"apiVersion":"example.io/v1alpha1","kind":"XAccount","metadata":{"name":"x"},
			"spec":{"region":"eu-central-1","imageParams":{"owners":"099720109477"},
			"imageFilters":[{"name":"name","values":["ubuntu-*"]}]}
		}`)}},
	}
	if _, err := f.RunFunction(context.Background(), req); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if seen == nil {
		t.Fatal("awsQuery was not called")
	}
	if seen.Region == nil || *seen.Region != "eu-central-1" {
		t.Errorf("regionRef not resolved: %v", seen.Region)
	}
	if seen.Parameters["owners"] != "099720109477" {
		t.Errorf("parametersRef not resolved: %v", seen.Parameters)
	}
	if _, stale := seen.Parameters["stale"]; stale {
		t.Error("parametersRef should override static parameters")
	}
	if len(seen.Filters) != 1 || seen.Filters[0].Name != "name" || len(seen.Filters[0].Values) != 1 || seen.Filters[0].Values[0] != "ubuntu-*" {
		t.Errorf("filtersRef not resolved/override failed: %v", seen.Filters)
	}
}

func TestRunFunctionIntervalSkip(t *testing.T) {
	called := false
	f := &Function{log: logging.NewNopLogger(), awsQuery: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
		called = true
		return nil, nil
	}}}
	// A recent lastQueryTime within the interval must cause a skip.
	now := time.Now().Format(time.RFC3339)
	observed := fmt.Sprintf(`{"apiVersion":"example.org/v1","kind":"XR","metadata":{"name":"test"},"status":{"data":[{"lastQueryTime":%q}]}}`, now)
	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test"},
		Input: resource.MustStructJSON(`{
			"apiVersion":"aws.fn.crossplane.io/v1beta1","kind":"Input",
			"queryType":"DescribeRegions","target":"status.data","queryIntervalMinutes":60
		}`),
		Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observed)}},
	}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Error("awsQuery should not be called within the query interval")
	}
	if !hasCondition(rsp, "FunctionSkip") {
		t.Errorf("expected a FunctionSkip condition, got: %v", rsp.GetConditions())
	}
}
