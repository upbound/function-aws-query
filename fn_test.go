package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/upbound/function-aws-query/input/v1beta1"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

// MockAWSQuery is a test double for AWSQueryInterface.
type MockAWSQuery struct {
	fn func(ctx context.Context, creds map[string][]byte, in *v1beta1.Input) (any, error)
}

func (m *MockAWSQuery) awsQuery(ctx context.Context, creds map[string][]byte, in *v1beta1.Input) (any, error) {
	return m.fn(ctx, creds, in)
}

const observedXR = `{"apiVersion":"example.org/v1","kind":"XR","metadata":{"name":"test"}}`

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestRunFunction(t *testing.T) {
	type args struct {
		req   *fnv1.RunFunctionRequest
		query AWSQueryInterface
	}
	type want struct {
		rsp *fnv1.RunFunctionResponse
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"StatusTarget": {
			reason: "A status target writes the projected result onto the desired XR status.",
			args: args{
				query: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
					return map[string]any{"account": "123456789012"}, nil
				}},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "aws.fn.crossplane.io/v1beta1",
						"kind": "Input",
						"queryType": "GetCallerIdentity",
						"target": "status.callerIdentity"
					}`),
					Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observedXR)}},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Desired: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(`{
						"apiVersion": "example.org/v1",
						"kind": "XR",
						"metadata": {"name": "test"},
						"status": {"callerIdentity": {"account": "123456789012"}}
					}`)}},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_NORMAL,
						Message:  `Query "GetCallerIdentity" succeeded`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
					Conditions: []*fnv1.Condition{{
						Type:   "FunctionSuccess",
						Status: fnv1.Status_STATUS_CONDITION_TRUE,
						Reason: "Success",
						Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
					}},
				},
			},
		},
		"ContextTarget": {
			reason: "A context target writes the projected result into the pipeline context.",
			args: args{
				query: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
					return []any{map[string]any{"name": "us-east-1"}}, nil
				}},
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "aws.fn.crossplane.io/v1beta1",
						"kind": "Input",
						"queryType": "DescribeRegions",
						"target": "context.regions"
					}`),
					Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observedXR)}},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta:    &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Desired: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observedXR)}},
					Context: mustStruct(t, map[string]any{
						"regions": []any{map[string]any{"name": "us-east-1"}},
					}),
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_NORMAL,
						Message:  `Query "DescribeRegions" succeeded`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
					Conditions: []*fnv1.Condition{{
						Type:   "FunctionSuccess",
						Status: fnv1.Status_STATUS_CONDITION_TRUE,
						Reason: "Success",
						Target: fnv1.Target_TARGET_COMPOSITE_AND_CLAIM.Enum(),
					}},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{log: logging.NewNopLogger(), awsQuery: tc.args.query}
			rsp, err := f.RunFunction(context.Background(), tc.args.req)

			if diff := cmp.Diff(tc.want.rsp, rsp, protocmp.Transform()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want rsp, +got rsp:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.err, err, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want err, +got err:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestRunFunctionInvalidTarget(t *testing.T) {
	called := false
	f := &Function{log: logging.NewNopLogger(), awsQuery: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
		called = true
		return nil, nil
	}}}
	req := &fnv1.RunFunctionRequest{
		Meta:     &fnv1.RequestMeta{Tag: "test"},
		Input:    resource.MustStructJSON(`{"apiVersion":"aws.fn.crossplane.io/v1beta1","kind":"Input","queryType":"GetCallerIdentity","target":"spec.foo"}`),
		Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(observedXR)}},
	}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Error("awsQuery should not be called for an invalid target")
	}
	if !hasFatal(rsp) {
		t.Errorf("expected a fatal result for an invalid target, got: %v", rsp.GetResults())
	}
}

func TestRunFunctionSkipWhenTargetHasData(t *testing.T) {
	called := false
	f := &Function{log: logging.NewNopLogger(), awsQuery: &MockAWSQuery{fn: func(_ context.Context, _ map[string][]byte, _ *v1beta1.Input) (any, error) {
		called = true
		return nil, nil
	}}}
	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test"},
		Input: resource.MustStructJSON(`{
			"apiVersion":"aws.fn.crossplane.io/v1beta1","kind":"Input",
			"queryType":"GetCallerIdentity","target":"status.callerIdentity","skipQueryWhenTargetHasData":true
		}`),
		Observed: &fnv1.State{Composite: &fnv1.Resource{Resource: resource.MustStructJSON(`{
			"apiVersion":"example.org/v1","kind":"XR","metadata":{"name":"test"},
			"status":{"callerIdentity":{"account":"123456789012"}}
		}`)}},
	}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if called {
		t.Error("awsQuery should not be called when the target already has data")
	}
	if !hasCondition(rsp, "FunctionSkip") {
		t.Errorf("expected a FunctionSkip condition, got: %v", rsp.GetConditions())
	}
}

func hasFatal(rsp *fnv1.RunFunctionResponse) bool {
	for _, r := range rsp.GetResults() {
		if r.GetSeverity() == fnv1.Severity_SEVERITY_FATAL {
			return true
		}
	}
	return false
}

func hasCondition(rsp *fnv1.RunFunctionResponse, t string) bool {
	for _, c := range rsp.GetConditions() {
		if c.GetType() == t {
			return true
		}
	}
	return false
}

func TestParseSharedCredsINI(t *testing.T) {
	cases := map[string]struct {
		in      string
		wantAK  string
		wantST  string
		wantReg string
		wantErr bool
	}{
		"Full": {
			in:      "[default]\naws_access_key_id = AKIA\naws_secret_access_key = secret\naws_session_token = tok\nregion = eu-central-1\n",
			wantAK:  "AKIA",
			wantST:  "tok",
			wantReg: "eu-central-1",
		},
		"NoSessionToken": {
			in:     "[default]\naws_access_key_id = AKIA\naws_secret_access_key = secret\n",
			wantAK: "AKIA",
		},
		"MissingSecret": {
			in:      "[default]\naws_access_key_id = AKIA\n",
			wantErr: true,
		},
		"Empty": {
			in:      "",
			wantErr: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ak, _, st, reg, err := parseSharedCredsINI([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ak != tc.wantAK || st != tc.wantST || reg != tc.wantReg {
				t.Errorf("got (ak=%q st=%q reg=%q), want (ak=%q st=%q reg=%q)", ak, st, reg, tc.wantAK, tc.wantST, tc.wantReg)
			}
		})
	}
}

func TestCSV(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"Empty":      {in: "", want: nil},
		"Spaces":     {in: "  ", want: nil},
		"Trim":       {in: "a, b ,c", want: []string{"a", "b", "c"}},
		"DropEmpty":  {in: "a,,b,", want: []string{"a", "b"}},
		"Single":     {in: "099720109477", want: []string{"099720109477"}},
		"Whitespace": {in: " x , y ", want: []string{"x", "y"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, csv(tc.in)); diff != "" {
				t.Errorf("csv(%q): -want +got:\n%s", tc.in, diff)
			}
		})
	}
}

func TestMatchesClientFilters(t *testing.T) {
	props := map[string]any{
		"VpcId": "vpc-123",
		"State": "available",
		"Tags": []any{
			map[string]any{"Key": "Environment", "Value": "prod"},
			map[string]any{"Key": "Team", "Value": "platform"},
		},
	}
	cases := map[string]struct {
		filters []v1beta1.Filter
		want    bool
	}{
		"NoFilters":      {filters: nil, want: true},
		"PropertyMatch":  {filters: []v1beta1.Filter{{Name: "State", Values: []string{"available"}}}, want: true},
		"PropertyNoData": {filters: []v1beta1.Filter{{Name: "Missing", Values: []string{"x"}}}, want: false},
		"PropertyValMis": {filters: []v1beta1.Filter{{Name: "State", Values: []string{"pending"}}}, want: false},
		"TagMatch":       {filters: []v1beta1.Filter{{Name: "tag:Environment", Values: []string{"prod"}}}, want: true},
		"TagMismatch":    {filters: []v1beta1.Filter{{Name: "tag:Environment", Values: []string{"dev"}}}, want: false},
		"TagMissing":     {filters: []v1beta1.Filter{{Name: "tag:Nope", Values: []string{"x"}}}, want: false},
		"AndOfFilters":   {filters: []v1beta1.Filter{{Name: "State", Values: []string{"available"}}, {Name: "tag:Team", Values: []string{"platform"}}}, want: true},
		"OrOfValues":     {filters: []v1beta1.Filter{{Name: "tag:Environment", Values: []string{"dev", "prod"}}}, want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := matchesClientFilters(props, tc.filters); got != tc.want {
				t.Errorf("matchesClientFilters() = %v, want %v", got, tc.want)
			}
		})
	}
}
