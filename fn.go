package main

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/upbound/function-aws-query/input/v1beta1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

// Function runs read-only AWS queries and writes the result to the XR status or
// the composition pipeline context.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	awsQuery AWSQueryInterface

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	// Ensure the observed XR (and its status) is propagated to the desired XR.
	if err := f.propagateDesiredXR(req, rsp); err != nil {
		return rsp, nil //nolint:nilerr // err is surfaced via rsp; the RPC itself must not fail
	}
	// Preserve the existing pipeline context.
	f.preserveContext(req, rsp)

	in, creds, err := f.parseInputAndCredentials(req, rsp)
	if err != nil {
		return rsp, nil //nolint:nilerr // err is surfaced via rsp; the RPC itself must not fail
	}

	// Resolve region from a reference if specified.
	if err := f.resolveRegionRef(req, in, rsp); err != nil {
		return rsp, nil //nolint:nilerr // err is surfaced via rsp; the RPC itself must not fail
	}

	if in.QueryType == "" {
		response.Fatal(rsp, errors.New("queryType is required"))
		return rsp, nil
	}

	if !f.isValidTarget(in.Target) {
		response.Fatal(rsp, errors.Errorf("Unrecognized target field: %s (must start with status. or context.)", in.Target))
		return rsp, nil
	}

	// Skip the query if configured to (target already has data, or interval limit).
	if f.shouldSkipQuery(req, in, rsp) {
		response.ConditionTrue(rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
		return rsp, nil
	}

	results, err := f.executeQuery(ctx, creds, in, rsp)
	if err != nil {
		return rsp, nil //nolint:nilerr // err is surfaced via rsp; the RPC itself must not fail
	}

	if err := f.processResults(req, in, results, rsp); err != nil {
		return rsp, nil //nolint:nilerr // err is surfaced via rsp; the RPC itself must not fail
	}

	response.ConditionTrue(rsp, "FunctionSuccess", "Success").TargetCompositeAndClaim()
	return rsp, nil
}

// parseInputAndCredentials parses the Input and extracts the aws-creds block.
func (f *Function) parseInputAndCredentials(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse) (*v1beta1.Input, map[string][]byte, error) {
	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "InternalError").
			WithMessage("Something went wrong.").
			TargetCompositeAndClaim()
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return nil, nil, err
	}

	if f.awsQuery == nil {
		f.awsQuery = &AWSQuery{log: f.log}
	}

	return in, getCreds(req), nil
}

// getCreds returns the decoded "aws-creds" credentials block (all keys). It is
// empty when no credentials block is supplied (IRSA / PodIdentity).
func getCreds(req *fnv1.RunFunctionRequest) map[string][]byte {
	out := map[string][]byte{}
	if c, ok := req.GetCredentials()[credentialsSecretName]; ok {
		maps.Copy(out, c.GetCredentialData().GetData())
	}
	return out
}

// resolveRegionRef resolves Input.RegionRef from status./context./spec. into
// Input.Region.
func (f *Function) resolveRegionRef(req *fnv1.RunFunctionRequest, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) error {
	if in.RegionRef == nil || *in.RegionRef == "" {
		return nil
	}
	ref := *in.RegionRef

	var root map[string]any
	switch {
	case strings.HasPrefix(ref, "context."):
		root = req.GetContext().AsMap()
		ref = strings.TrimPrefix(ref, "context.")
	case strings.HasPrefix(ref, "status."):
		xrStatus, _, err := f.getXRAndStatus(req)
		if err != nil {
			response.Fatal(rsp, err)
			return err
		}
		root = xrStatus
		ref = strings.TrimPrefix(ref, "status.")
	case strings.HasPrefix(ref, "spec."):
		oxr, err := request.GetObservedCompositeResource(req)
		if err != nil {
			response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
			return err
		}
		spec := map[string]any{}
		_ = oxr.Resource.GetValueInto("spec", &spec)
		root = spec
		ref = strings.TrimPrefix(ref, "spec.")
	default:
		err := errors.Errorf("Unrecognized RegionRef field: %s (must start with status., context., or spec.)", *in.RegionRef)
		response.Fatal(rsp, err)
		return err
	}

	if v, ok := GetNestedKey(root, ref); ok && v != "" {
		in.Region = &v
	}
	return nil
}

// executeQuery runs the AWS query.
func (f *Function) executeQuery(ctx context.Context, creds map[string][]byte, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) (any, error) {
	results, err := f.awsQuery.awsQuery(ctx, creds, in)
	if err != nil {
		response.Fatal(rsp, err)
		f.log.Info("FAILURE", "queryType", in.QueryType, "error", err.Error())
		return nil, err
	}
	f.log.Info("Query succeeded", "queryType", in.QueryType)
	response.Normalf(rsp, "Query %q succeeded", in.QueryType)
	return results, nil
}

// processResults writes the query result to the configured target.
func (f *Function) processResults(req *fnv1.RunFunctionRequest, in *v1beta1.Input, results any, rsp *fnv1.RunFunctionResponse) error {
	switch {
	case strings.HasPrefix(in.Target, "status."):
		if err := f.putQueryResultToStatus(req, rsp, in, results); err != nil {
			response.Fatal(rsp, err)
			return err
		}
	case strings.HasPrefix(in.Target, "context."):
		if err := f.putQueryResultToContext(req, rsp, in, results); err != nil {
			response.Fatal(rsp, err)
			return err
		}
	default:
		response.Fatal(rsp, errors.Errorf("Unrecognized target field: %s", in.Target))
		return errors.New("unrecognized target field")
	}
	return nil
}

// putQueryResultToStatus writes the result to the desired XR status.
func (f *Function) putQueryResultToStatus(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse, in *v1beta1.Input, results any) error {
	xrStatus, dxr, err := f.getXRAndStatus(req)
	if err != nil {
		return err
	}

	resultData := withQueryInterval(results, in, f.log)

	statusField := strings.TrimPrefix(in.Target, "status.")
	if err := SetNestedKey(xrStatus, statusField, resultData); err != nil {
		return errors.Wrapf(err, "cannot set status field %s", statusField)
	}

	if err := dxr.Resource.SetValue("status", xrStatus); err != nil {
		return errors.Wrap(err, "cannot write updated status back into composite resource")
	}

	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		return errors.Wrapf(err, "cannot set desired composite resource in %T", rsp)
	}
	return nil
}

// putQueryResultToContext writes the result to the composition pipeline context.
func (f *Function) putQueryResultToContext(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse, in *v1beta1.Input, results any) error {
	contextField := strings.TrimPrefix(in.Target, "context.")
	data, err := structpb.NewValue(results)
	if err != nil {
		return errors.Wrap(err, "cannot convert results to structpb.Value")
	}

	contextMap := req.GetContext().AsMap()
	if err := SetNestedKey(contextMap, contextField, data.AsInterface()); err != nil {
		return errors.Wrap(err, "failed to update context key")
	}

	updatedContext, err := structpb.NewStruct(contextMap)
	if err != nil {
		return errors.Wrap(err, "failed to serialize updated context")
	}
	rsp.Context = updatedContext
	return nil
}

// withQueryInterval appends a lastQueryTime marker when QueryIntervalMinutes is
// set, so interval-based skipping can find the last execution time.
func withQueryInterval(results any, in *v1beta1.Input, log logging.Logger) any {
	if in.QueryIntervalMinutes == nil || *in.QueryIntervalMinutes <= 0 {
		return results
	}
	now := time.Now().Format(time.RFC3339)
	switch data := results.(type) {
	case []any:
		return append(data, map[string]any{"lastQueryTime": now})
	case map[string]any:
		data["lastQueryTime"] = now
		return data
	default:
		log.Debug("result is neither array nor map; cannot add lastQueryTime")
		return results
	}
}

// getXRAndStatus retrieves status and the desired XR, initializing the desired
// XR from the observed XR if needed.
func (f *Function) getXRAndStatus(req *fnv1.RunFunctionRequest) (map[string]any, *resource.Composite, error) {
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot get observed composite resource")
	}

	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot get desired composite resource")
	}

	xrStatus := make(map[string]any)

	if dxr.Resource.GetKind() == "" {
		dxr.Resource.SetAPIVersion(oxr.Resource.GetAPIVersion())
		dxr.Resource.SetKind(oxr.Resource.GetKind())
		dxr.Resource.SetName(oxr.Resource.GetName())
	}

	// Prefer status already present on the desired XR (pipeline changes).
	if dxr.Resource.GetKind() != "" {
		if err := dxr.Resource.GetValueInto("status", &xrStatus); err == nil && len(xrStatus) > 0 {
			return xrStatus, dxr, nil
		}
		f.log.Debug("Cannot get status from desired XR or it's empty")
	}

	// Fall back to the observed XR status.
	if err := oxr.Resource.GetValueInto("status", &xrStatus); err != nil {
		f.log.Debug("Cannot get status from observed XR")
	}

	return xrStatus, dxr, nil
}

// propagateDesiredXR ensures the desired XR is propagated without losing status.
func (f *Function) propagateDesiredXR(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse) error {
	xrStatus, dxr, err := f.getXRAndStatus(req)
	if err != nil {
		response.Fatal(rsp, err)
		return err
	}

	if len(xrStatus) > 0 {
		if err := dxr.Resource.SetValue("status", xrStatus); err != nil {
			f.log.Info("Error setting status in desired XR", "error", err)
			return err
		}
	}

	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composite resource in %T", rsp))
		return err
	}
	return nil
}

// preserveContext copies the existing request context into the response.
func (f *Function) preserveContext(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse) {
	if existing := req.GetContext(); existing != nil {
		rsp.Context = existing
	}
}

// isValidTarget reports whether target is a supported destination.
func (f *Function) isValidTarget(target string) bool {
	return strings.HasPrefix(target, "status.") || strings.HasPrefix(target, "context.")
}

// shouldSkipQuery reports whether the query should be skipped.
func (f *Function) shouldSkipQuery(req *fnv1.RunFunctionRequest, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) bool {
	if f.shouldSkipQueryDueToInterval(req, in, rsp) {
		return true
	}

	skip := false
	if in.SkipQueryWhenTargetHasData != nil {
		skip = *in.SkipQueryWhenTargetHasData
	}
	if !skip {
		return false
	}

	switch {
	case strings.HasPrefix(in.Target, "status."):
		return f.checkStatusTargetHasData(req, in, rsp)
	case strings.HasPrefix(in.Target, "context."):
		return f.checkContextTargetHasData(req, in, rsp)
	}
	return false
}

func (f *Function) checkStatusTargetHasData(req *fnv1.RunFunctionRequest, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) bool {
	xrStatus, _, err := f.getXRAndStatus(req)
	if err != nil {
		response.Fatal(rsp, err)
		return true
	}
	statusField := strings.TrimPrefix(in.Target, "status.")
	if hasData, _ := targetHasData(xrStatus, statusField); hasData {
		f.log.Info("Target already has data, skipping query", "target", in.Target)
		response.ConditionTrue(rsp, "FunctionSkip", "SkippedQuery").
			WithMessage("Target already has data, skipped query to avoid throttling").
			TargetCompositeAndClaim()
		return true
	}
	return false
}

func (f *Function) checkContextTargetHasData(req *fnv1.RunFunctionRequest, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) bool {
	contextMap := req.GetContext().AsMap()
	contextField := strings.TrimPrefix(in.Target, "context.")
	if hasData, _ := targetHasData(contextMap, contextField); hasData {
		f.log.Info("Target already has data, skipping query", "target", in.Target)
		response.ConditionTrue(rsp, "FunctionSkip", "SkippedQuery").
			WithMessage("Target already has data, skipped query to avoid throttling").
			TargetCompositeAndClaim()
		return true
	}
	return false
}

func (f *Function) shouldSkipQueryDueToInterval(req *fnv1.RunFunctionRequest, in *v1beta1.Input, rsp *fnv1.RunFunctionResponse) bool {
	if in.QueryIntervalMinutes == nil || *in.QueryIntervalMinutes <= 0 {
		return false
	}
	if !strings.HasPrefix(in.Target, "status.") {
		return false
	}

	targetData, err := f.getTargetData(req, in)
	if err != nil {
		return false
	}
	lastQueryTime, err := extractLastQueryTime(targetData)
	if err != nil {
		return false
	}
	return f.checkIntervalLimit(lastQueryTime, *in.QueryIntervalMinutes, in.Target, rsp)
}

func (f *Function) getTargetData(req *fnv1.RunFunctionRequest, in *v1beta1.Input) (any, error) {
	xrStatus, _, err := f.getXRAndStatus(req)
	if err != nil {
		return nil, err
	}
	statusField := strings.TrimPrefix(in.Target, "status.")
	parts, err := ParseNestedKey(statusField)
	if err != nil {
		return nil, err
	}
	current := any(xrStatus)
	for _, k := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, errors.New("invalid nested structure")
		}
		next, exists := m[k]
		if !exists {
			return nil, errors.New("no existing data")
		}
		current = next
	}
	return current, nil
}

func extractLastQueryTime(targetData any) (time.Time, error) {
	switch data := targetData.(type) {
	case []any:
		for i := len(data) - 1; i >= 0; i-- {
			el, ok := data[i].(map[string]any)
			if !ok {
				continue
			}
			if s, ok := el["lastQueryTime"].(string); ok {
				return time.Parse(time.RFC3339, s)
			}
		}
		return time.Time{}, errors.New("no lastQueryTime element found in array")
	case map[string]any:
		s, ok := data["lastQueryTime"].(string)
		if !ok {
			return time.Time{}, errors.New("no lastQueryTime field")
		}
		return time.Parse(time.RFC3339, s)
	}
	return time.Time{}, errors.New("target data is neither array nor map")
}

func (f *Function) checkIntervalLimit(lastQueryTime time.Time, intervalMinutes int, target string, rsp *fnv1.RunFunctionResponse) bool {
	elapsed := time.Since(lastQueryTime)
	if elapsed < time.Duration(intervalMinutes)*time.Minute {
		f.log.Info("Skipping query due to interval limit", "target", target, "intervalMinutes", intervalMinutes, "elapsedMinutes", elapsed.Minutes())
		response.ConditionTrue(rsp, "FunctionSkip", "IntervalLimit").
			WithMessage(fmt.Sprintf("Query skipped due to interval limit (%d minutes)", intervalMinutes)).
			TargetCompositeAndClaim()
		return true
	}
	return false
}

// targetHasData reports whether the nested key holds meaningful data.
func targetHasData(data map[string]any, key string) (bool, error) {
	parts, err := ParseNestedKey(key)
	if err != nil {
		return false, err
	}
	current := any(data)
	for _, k := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return false, nil
		}
		next, exists := m[k]
		if !exists {
			return false, nil
		}
		current = next
	}
	switch v := current.(type) {
	case nil:
		return false, nil
	case map[string]any:
		return len(v) > 0, nil
	case []any:
		return len(v) > 0, nil
	case string:
		return v != "", nil
	default:
		return true, nil
	}
}

// ParseNestedKey splits a key into parts, supporting dot and bracket notation.
func ParseNestedKey(key string) ([]string, error) {
	regex := regexp.MustCompile(`\[([^\[\]]+)\]|([^.\[\]]+)`)
	matches := regex.FindAllStringSubmatch(key, -1)
	var parts []string
	for _, match := range matches {
		switch {
		case match[1] != "":
			parts = append(parts, match[1])
		case match[2] != "":
			parts = append(parts, match[2])
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("invalid key")
	}
	return parts, nil
}

// GetNestedKey retrieves a nested string value using dot/bracket notation.
func GetNestedKey(root map[string]any, key string) (string, bool) {
	parts, err := ParseNestedKey(key)
	if err != nil {
		return "", false
	}
	current := any(root)
	for _, k := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		next, exists := m[k]
		if !exists {
			return "", false
		}
		current = next
	}
	if s, ok := current.(string); ok {
		return s, true
	}
	return "", false
}

// SetNestedKey sets value at a nested key, creating intermediate maps.
func SetNestedKey(root map[string]any, key string, value any) error {
	parts, err := ParseNestedKey(key)
	if err != nil {
		return err
	}
	current := root
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
			return nil
		}
		if next, exists := current[part]; exists {
			m, ok := next.(map[string]any)
			if !ok {
				return fmt.Errorf("key %q exists but is not a map", part)
			}
			current = m
			continue
		}
		m := make(map[string]any)
		current[part] = m
		current = m
	}
	return nil
}
