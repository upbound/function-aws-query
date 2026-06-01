package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/upbound/function-aws-query/input/v1beta1"
	ini "gopkg.in/ini.v1"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
)

// credentialsSecretName is the name of the function credentials block that
// carries the AWS credentials (shared-credentials INI under key "credentials",
// and an optional web identity token under key "token").
const credentialsSecretName = "aws-creds"

// AWSQueryInterface is the mockable seam between RunFunction and the real AWS
// calls. creds holds the decoded "aws-creds" credentials block: key
// "credentials" (shared-credentials INI) and optional key "token" (WebIdentity).
// It is empty/absent for IRSA and PodIdentity.
type AWSQueryInterface interface {
	awsQuery(ctx context.Context, creds map[string][]byte, in *v1beta1.Input) (any, error)
}

// AWSQuery is the concrete AWSQueryInterface implementation.
type AWSQuery struct {
	log logging.Logger
}

// handler executes one AWS read operation against a resolved config and returns
// JSON-ready (structpb-safe) data.
type handler func(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error)

func (q *AWSQuery) registry() map[string]handler {
	return map[string]handler{
		"GetCallerIdentity":         q.getCallerIdentity,
		"DescribeRegions":           q.describeRegions,
		"DescribeAvailabilityZones": q.describeAvailabilityZones,
		"DescribeImages":            q.describeImages,
		"ListServiceQuotas":         q.listServiceQuotas,
		"GetServiceQuota":           q.getServiceQuota,
		"ListResources":             q.listResources,
		"GetResources":              q.getResources,
	}
}

// awsQuery resolves credentials, then dispatches to the handler for QueryType.
func (q *AWSQuery) awsQuery(ctx context.Context, creds map[string][]byte, in *v1beta1.Input) (any, error) {
	cfg, err := buildAWSConfig(ctx, creds, in)
	if err != nil {
		return nil, err
	}
	h, ok := q.registry()[in.QueryType]
	if !ok {
		return nil, errors.Errorf("unsupported queryType: %s", in.QueryType)
	}
	return h(ctx, cfg, in)
}

// buildAWSConfig resolves a base credentials provider per Identity.Source
// (mirroring provider-upjet-aws), then layers the assumeRoleChain on top.
func buildAWSConfig(ctx context.Context, creds map[string][]byte, in *v1beta1.Input) (aws.Config, error) { //nolint:gocyclo // straight-line credential-source selection; splitting hurts readability
	opts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() aws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 5) }),
	}
	if region := resolveRegion(creds, in); region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	source := v1beta1.IdentitySourceSecret
	var webIdentity *v1beta1.WebIdentity
	var chain []v1beta1.AssumeRole
	if in.Identity != nil {
		if in.Identity.Source != "" {
			source = in.Identity.Source
		}
		webIdentity = in.Identity.WebIdentity
		chain = in.Identity.AssumeRoleChain
	}

	switch source {
	case v1beta1.IdentitySourceSecret:
		// Shared-credentials INI, byte-identical to provider-upjet-aws Secret source.
		ak, sk, st, _, err := parseSharedCredsINI(creds["credentials"])
		if err != nil {
			return aws.Config{}, err
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, st)))
	case v1beta1.IdentitySourceIRSA, v1beta1.IdentitySourcePodIdentity:
		// No secret: the SDK default credential chain resolves the web identity
		// token file (IRSA) or the container credentials endpoint (Pod Identity).
		// The function pod's ServiceAccount carries the role.
	case v1beta1.IdentitySourceWebIdentity:
		if webIdentity == nil || webIdentity.RoleARN == "" {
			return aws.Config{}, errors.New("identity.webIdentity.roleARN is required for the WebIdentity source")
		}
		base, err := config.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return aws.Config{}, errors.Wrap(err, "cannot load base AWS config for WebIdentity")
		}
		retriever, err := webIdentityTokenRetriever(creds["token"], webIdentity)
		if err != nil {
			return aws.Config{}, err
		}
		provider := stscreds.NewWebIdentityRoleProvider(sts.NewFromConfig(base), webIdentity.RoleARN, retriever,
			func(o *stscreds.WebIdentityRoleOptions) {
				if webIdentity.RoleSessionName != "" {
					o.RoleSessionName = webIdentity.RoleSessionName
				}
			})
		opts = append(opts, config.WithCredentialsProvider(aws.NewCredentialsCache(provider)))
	default:
		return aws.Config{}, errors.Errorf("unsupported identity source: %s", source)
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, errors.Wrap(err, "cannot load AWS config")
	}

	// assumeRoleChain: assume each role in order, each using the previous step's credentials.
	for _, r := range chain {
		prev := cfg
		role := r
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(prev), role.RoleARN,
			func(o *stscreds.AssumeRoleOptions) {
				if role.ExternalID != nil {
					o.ExternalID = role.ExternalID
				}
				if role.RoleSessionName != nil {
					o.RoleSessionName = *role.RoleSessionName
				}
				o.Tags = toSTSTags(role.Tags)
				o.TransitiveTagKeys = role.TransitiveTagKeys
			}))
	}
	return cfg, nil
}

// staticIdentityToken is an in-memory stscreds.IdentityTokenRetriever backed by
// token bytes delivered through the function credentials block.
type staticIdentityToken []byte

// GetIdentityToken returns the token bytes.
func (t staticIdentityToken) GetIdentityToken() ([]byte, error) { return t, nil }

func webIdentityTokenRetriever(tokenSecret []byte, wi *v1beta1.WebIdentity) (stscreds.IdentityTokenRetriever, error) {
	source := "Secret"
	if wi.TokenConfig != nil && wi.TokenConfig.Source != "" {
		source = wi.TokenConfig.Source
	}
	switch source {
	case "Secret":
		if len(tokenSecret) == 0 {
			return nil, errors.New("WebIdentity token is empty (expected key \"token\" in the aws-creds credentials block)")
		}
		return staticIdentityToken(tokenSecret), nil
	case "Filesystem":
		if wi.TokenConfig == nil || wi.TokenConfig.FSPath == nil || *wi.TokenConfig.FSPath == "" {
			return nil, errors.New("identity.webIdentity.tokenConfig.fsPath is required for the Filesystem token source")
		}
		return stscreds.IdentityTokenFile(*wi.TokenConfig.FSPath), nil
	}
	return nil, errors.Errorf("unsupported tokenConfig.source: %s", source)
}

// resolveRegion picks the region: Input.Region (RegionRef is resolved into it by
// fn.go) -> credential profile region -> AWS_REGION env (handled by the SDK when
// we leave it unset) -> us-east-1 for global-ish calls.
func resolveRegion(creds map[string][]byte, in *v1beta1.Input) string {
	if in.Region != nil && *in.Region != "" {
		return *in.Region
	}
	if r := iniRegion(creds["credentials"]); r != "" {
		return r
	}
	// AWS_REGION / AWS_DEFAULT_REGION are picked up automatically by
	// LoadDefaultConfig when WithRegion is not supplied.
	if isGlobalQuery(in.QueryType) {
		return "us-east-1"
	}
	return ""
}

func isGlobalQuery(queryType string) bool {
	switch queryType {
	case "GetCallerIdentity", "DescribeRegions":
		return true
	}
	return false
}

// getCallerIdentity returns the AWS account, ARN, and user ID (STS).
func (q *AWSQuery) getCallerIdentity(ctx context.Context, cfg aws.Config, _ *v1beta1.Input) (any, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, errors.Wrap(err, "GetCallerIdentity failed")
	}
	return map[string]any{
		"account": aws.ToString(out.Account),
		"arn":     aws.ToString(out.Arn),
		"userId":  aws.ToString(out.UserId),
	}, nil
}

// describeRegions lists AWS regions (EC2).
func (q *AWSQuery) describeRegions(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	input := &ec2.DescribeRegionsInput{Filters: toEC2Filters(in.Filters)}
	if b, _ := strconv.ParseBool(in.Parameters["allRegions"]); b {
		input.AllRegions = aws.Bool(true)
	}
	out, err := ec2.NewFromConfig(cfg).DescribeRegions(ctx, input)
	if err != nil {
		return nil, errors.Wrap(err, "DescribeRegions failed")
	}
	res := []any{}
	for _, r := range out.Regions {
		res = append(res, map[string]any{
			"name":        aws.ToString(r.RegionName),
			"endpoint":    aws.ToString(r.Endpoint),
			"optInStatus": aws.ToString(r.OptInStatus),
		})
	}
	return res, nil
}

// describeAvailabilityZones lists AZs in the target region (EC2).
func (q *AWSQuery) describeAvailabilityZones(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("DescribeAvailabilityZones")
	}
	input := &ec2.DescribeAvailabilityZonesInput{Filters: toEC2Filters(in.Filters)}
	if b, _ := strconv.ParseBool(in.Parameters["allAvailabilityZones"]); b {
		input.AllAvailabilityZones = aws.Bool(true)
	}
	out, err := ec2.NewFromConfig(cfg).DescribeAvailabilityZones(ctx, input)
	if err != nil {
		return nil, errors.Wrap(err, "DescribeAvailabilityZones failed")
	}
	res := []any{}
	for _, z := range out.AvailabilityZones {
		res = append(res, map[string]any{
			"name":       aws.ToString(z.ZoneName),
			"zoneId":     aws.ToString(z.ZoneId),
			"state":      string(z.State),
			"regionName": aws.ToString(z.RegionName),
			"zoneType":   aws.ToString(z.ZoneType),
			"groupName":  aws.ToString(z.GroupName),
		})
	}
	return res, nil
}

// describeImages looks up AMIs (EC2). Requires owners, filters, or imageIds.
func (q *AWSQuery) describeImages(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("DescribeImages")
	}
	owners := csv(in.Parameters["owners"])
	imageIDs := csv(in.Parameters["imageIds"])
	if len(owners) == 0 && len(in.Filters) == 0 && len(imageIDs) == 0 {
		return nil, errors.New("DescribeImages requires parameters.owners, filters, or parameters.imageIds to avoid returning all public AMIs")
	}
	input := &ec2.DescribeImagesInput{Filters: toEC2Filters(in.Filters)}
	if len(owners) > 0 {
		input.Owners = owners
	}
	if len(imageIDs) > 0 {
		input.ImageIds = imageIDs
	}
	out, err := ec2.NewFromConfig(cfg).DescribeImages(ctx, input)
	if err != nil {
		return nil, errors.Wrap(err, "DescribeImages failed")
	}
	res := []any{}
	for _, im := range out.Images {
		res = append(res, map[string]any{
			"imageId":        aws.ToString(im.ImageId),
			"name":           aws.ToString(im.Name),
			"ownerId":        aws.ToString(im.OwnerId),
			"creationDate":   aws.ToString(im.CreationDate),
			"architecture":   string(im.Architecture),
			"state":          string(im.State),
			"rootDeviceType": string(im.RootDeviceType),
			"description":    aws.ToString(im.Description),
		})
	}
	return res, nil
}

// listServiceQuotas lists quotas for a service (ServiceQuotas, paginated).
func (q *AWSQuery) listServiceQuotas(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("ListServiceQuotas")
	}
	serviceCode := in.Parameters["serviceCode"]
	if serviceCode == "" {
		return nil, errors.New("ListServiceQuotas requires parameters.serviceCode (e.g. \"ec2\")")
	}
	client := servicequotas.NewFromConfig(cfg)
	p := servicequotas.NewListServiceQuotasPaginator(client, &servicequotas.ListServiceQuotasInput{ServiceCode: aws.String(serviceCode)})
	res := []any{}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "ListServiceQuotas failed")
		}
		for _, quota := range page.Quotas {
			res = append(res, quotaToMap(quota))
		}
	}
	return res, nil
}

// getServiceQuota gets a single quota (ServiceQuotas).
func (q *AWSQuery) getServiceQuota(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("GetServiceQuota")
	}
	serviceCode, quotaCode := in.Parameters["serviceCode"], in.Parameters["quotaCode"]
	if serviceCode == "" || quotaCode == "" {
		return nil, errors.New("GetServiceQuota requires parameters.serviceCode and parameters.quotaCode")
	}
	out, err := servicequotas.NewFromConfig(cfg).GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
		ServiceCode: aws.String(serviceCode),
		QuotaCode:   aws.String(quotaCode),
	})
	if err != nil {
		return nil, errors.Wrap(err, "GetServiceQuota failed")
	}
	if out.Quota == nil {
		return map[string]any{}, nil
	}
	return quotaToMap(*out.Quota), nil
}

func quotaToMap(quota sqtypes.ServiceQuota) map[string]any {
	m := map[string]any{
		"quotaCode":   aws.ToString(quota.QuotaCode),
		"quotaName":   aws.ToString(quota.QuotaName),
		"unit":        aws.ToString(quota.Unit),
		"adjustable":  quota.Adjustable,
		"globalQuota": quota.GlobalQuota,
	}
	if quota.Value != nil {
		m["value"] = *quota.Value
	}
	return m
}

// listResources lists resources of a CloudFormation-modeled type (Cloud Control,
// paginated) and applies client-side filters. ListResources itself returns only
// the primary identifier (plus a thin subset of properties) for most types, so
// by default each resource is hydrated with GetResource to obtain the full model
// (including Tags). Set parameters.hydrate=false to skip hydration and return
// just the listed identifiers — faster, but tag/attribute filters won't match.
func (q *AWSQuery) listResources(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("ListResources")
	}
	typeName := in.Parameters["typeName"]
	if typeName == "" {
		return nil, errors.New("ListResources requires parameters.typeName (e.g. \"AWS::EC2::VPC\")")
	}
	roleArn := in.Parameters["roleArn"]
	hydrate := true
	if v, ok := in.Parameters["hydrate"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			hydrate = b
		}
	}

	client := cloudcontrol.NewFromConfig(cfg)
	input := &cloudcontrol.ListResourcesInput{TypeName: aws.String(typeName)}
	if rm := in.Parameters["resourceModel"]; rm != "" {
		input.ResourceModel = aws.String(rm)
	}
	if roleArn != "" {
		input.RoleArn = aws.String(roleArn)
	}

	p := cloudcontrol.NewListResourcesPaginator(client, input)
	res := []any{}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "ListResources failed")
		}
		for _, rd := range page.ResourceDescriptions {
			id := aws.ToString(rd.Identifier)
			props := parseCloudControlProps(rd.Properties, q.log)
			if hydrate {
				props, err = q.getCloudControlResource(ctx, client, typeName, id, roleArn)
				if err != nil {
					return nil, err
				}
			}
			if !matchesClientFilters(props, in.Filters) {
				continue
			}
			res = append(res, map[string]any{
				"identifier": id,
				"properties": props,
			})
		}
	}
	return res, nil
}

// getCloudControlResource fetches the full property model for a single resource.
func (q *AWSQuery) getCloudControlResource(ctx context.Context, client *cloudcontrol.Client, typeName, identifier, roleArn string) (map[string]any, error) {
	in := &cloudcontrol.GetResourceInput{TypeName: aws.String(typeName), Identifier: aws.String(identifier)}
	if roleArn != "" {
		in.RoleArn = aws.String(roleArn)
	}
	out, err := client.GetResource(ctx, in)
	if err != nil {
		return nil, errors.Wrapf(err, "GetResource %s %s failed", typeName, identifier)
	}
	if out.ResourceDescription == nil {
		return map[string]any{}, nil
	}
	return parseCloudControlProps(out.ResourceDescription.Properties, q.log), nil
}

// parseCloudControlProps unmarshals a Cloud Control Properties JSON string into a
// structpb-safe map.
func parseCloudControlProps(properties *string, log logging.Logger) map[string]any {
	props := map[string]any{}
	if properties != nil {
		if err := json.Unmarshal([]byte(*properties), &props); err != nil {
			log.Debug("cannot parse Cloud Control resource properties", "error", err)
		}
	}
	return props
}

// getResources finds resources by tag/type (Resource Groups Tagging API,
// paginated), returning ARN + tags.
func (q *AWSQuery) getResources(ctx context.Context, cfg aws.Config, in *v1beta1.Input) (any, error) {
	if cfg.Region == "" {
		return nil, errRegionRequired("GetResources")
	}
	input := &resourcegroupstaggingapi.GetResourcesInput{
		TagFilters:          toTagFilters(in.Filters),
		ResourceTypeFilters: csv(in.Parameters["resourceTypeFilters"]),
	}
	p := resourcegroupstaggingapi.NewGetResourcesPaginator(resourcegroupstaggingapi.NewFromConfig(cfg), input)
	res := []any{}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "GetResources failed")
		}
		for _, m := range page.ResourceTagMappingList {
			tags := map[string]any{}
			for _, t := range m.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			res = append(res, map[string]any{
				"arn":  aws.ToString(m.ResourceARN),
				"tags": tags,
			})
		}
	}
	return res, nil
}

// matchesClientFilters applies Input.Filters client-side to a Cloud Control
// resource's properties. Filters AND together; values within a filter OR. A
// filter name of "tag:Key" matches against the resource's Tags property.
func matchesClientFilters(props map[string]any, filters []v1beta1.Filter) bool {
	for _, f := range filters {
		var actual string
		var found bool
		switch {
		case strings.HasPrefix(f.Name, "tag:"):
			actual, found = tagFromProps(props, strings.TrimPrefix(f.Name, "tag:"))
		default:
			if v, ok := props[f.Name]; ok {
				actual, found = fmt.Sprintf("%v", v), true
			}
		}
		if !found || !contains(f.Values, actual) {
			return false
		}
	}
	return true
}

// tagFromProps reads a tag value from a Cloud Control properties map, where Tags
// are typically modeled as an array of {Key, Value} objects.
func tagFromProps(props map[string]any, key string) (string, bool) {
	raw, ok := props["Tags"]
	if !ok {
		return "", false
	}
	arr, ok := raw.([]any)
	if !ok {
		return "", false
	}
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprintf("%v", m["Key"]) == key {
			return fmt.Sprintf("%v", m["Value"]), true
		}
	}
	return "", false
}

func toEC2Filters(filters []v1beta1.Filter) []ec2types.Filter {
	if len(filters) == 0 {
		return nil
	}
	out := make([]ec2types.Filter, 0, len(filters))
	for _, f := range filters {
		out = append(out, ec2types.Filter{Name: aws.String(f.Name), Values: f.Values})
	}
	return out
}

func toTagFilters(filters []v1beta1.Filter) []rgttypes.TagFilter {
	if len(filters) == 0 {
		return nil
	}
	out := make([]rgttypes.TagFilter, 0, len(filters))
	for _, f := range filters {
		out = append(out, rgttypes.TagFilter{Key: aws.String(f.Name), Values: f.Values})
	}
	return out
}

func toSTSTags(tags map[string]string) []ststypes.Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]ststypes.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, ststypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// parseSharedCredsINI parses an AWS shared-credentials INI from bytes (no temp
// file), reading the [default] profile. Compatible with provider-upjet-aws.
func parseSharedCredsINI(b []byte) (accessKeyID, secretAccessKey, sessionToken, region string, err error) {
	if len(b) == 0 {
		return "", "", "", "", errors.New("aws-creds secret is empty (expected key \"credentials\" with a shared-credentials INI)")
	}
	f, err := ini.Load(b)
	if err != nil {
		return "", "", "", "", errors.Wrap(err, "cannot parse shared-credentials INI")
	}
	s := f.Section("default")
	accessKeyID = s.Key("aws_access_key_id").String()
	secretAccessKey = s.Key("aws_secret_access_key").String()
	sessionToken = s.Key("aws_session_token").String()
	region = s.Key("region").String()
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", "", "", errors.New("shared-credentials INI [default] is missing aws_access_key_id or aws_secret_access_key")
	}
	return accessKeyID, secretAccessKey, sessionToken, region, nil
}

func iniRegion(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	f, err := ini.Load(b)
	if err != nil {
		return ""
	}
	return f.Section("default").Key("region").String()
}

func csv(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func contains(values []string, v string) bool {
	return slices.Contains(values, v)
}

func errRegionRequired(op string) error {
	return errors.Errorf("%s requires a region; set spec.region, regionRef, the credential profile region, or AWS_REGION", op)
}
