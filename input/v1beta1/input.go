// Package v1beta1 contains the input type for this Function
// +kubebuilder:object:generate=true
// +groupName=aws.fn.crossplane.io
// +versionName=v1beta1
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This isn't a custom resource, in the sense that we never install its CRD.
// It is a KRM-like object, so we generate a CRD to describe its schema.

// Input configures function-aws-query: which read-only AWS query to run, how to
// authenticate, and where to store the result.
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:categories=crossplane
type Input struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// QueryType selects the AWS read operation to perform.
	// +kubebuilder:validation:Enum=GetCallerIdentity;DescribeRegions;DescribeAvailabilityZones;DescribeImages;DescribeRouteTables;DescribeSubnets;DescribeSecurityGroupRules;ListServiceQuotas;GetServiceQuota;ListResources;GetResources
	QueryType string `json:"queryType"`

	// Region to target. Optional for global-ish calls (GetCallerIdentity,
	// DescribeRegions). If empty, falls back to RegionRef, the credential
	// profile region, then the AWS_REGION environment variable.
	// +optional
	Region *string `json:"region,omitempty"`

	// RegionRef resolves the region from a status./context./spec. field path.
	// +optional
	RegionRef *string `json:"regionRef,omitempty"`

	// Filters are name/values pairs. Their interpretation depends on QueryType.
	// For every EC2 query these are native EC2 filter names, applied
	// server-side, and an unrecognised NAME is fatal - so they are listed per
	// query type rather than generically. They are NOT interchangeable:
	//   - DescribeRegions, DescribeAvailabilityZones, DescribeImages:
	//     "tag:Name", "state", "architecture", ...
	//   - DescribeRouteTables (required): "vpc-id", "route-table-id",
	//     "association.subnet-id", "tag:<key>", ...
	//   - DescribeSubnets (required): "vpc-id", "subnet-id",
	//     "availability-zone", "tag:<key>", ...
	//   - DescribeSecurityGroupRules (required): "group-id",
	//     "security-group-rule-id", "tag:<key>". This operation does NOT
	//     accept "vpc-id".
	//   - GetResources (Tagging API): each entry is a tag filter where name is
	//     the tag key and values are the tag values (server-side).
	//   - ListResources (Cloud Control): client-side property match where name
	//     is a top-level property or "tag:Key".
	// +optional
	Filters []Filter `json:"filters,omitempty"`

	// FiltersRef resolves Filters from a status./context./spec. field path that
	// holds a list of {name, values} objects — enabling dynamic queries driven
	// by a prior pipeline step. Overrides Filters when set.
	// +optional
	FiltersRef *string `json:"filtersRef,omitempty"`

	// Parameters carries scalar/string-list args specific to each QueryType:
	//   allRegions, allAvailabilityZones (bool); owners, imageIds (csv)        [EC2]
	//   serviceCode, quotaCode                                                 [ServiceQuotas]
	//   typeName (e.g. AWS::EC2::VPC), resourceModel (json), roleArn,
	//     hydrate (bool, default true: GetResource each item for full props)  [Cloud Control]
	//   resourceTypeFilters (csv, e.g. "ec2:vpc,ec2:subnet")                   [Tagging API]
	// Comma-separated values are split by the handler.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ParametersRef resolves Parameters from a status./context./spec. field path
	// that holds a string map. Overrides Parameters when set.
	// +optional
	ParametersRef *string `json:"parametersRef,omitempty"`

	// Target where to store the query result. Must start with "status." or
	// "context.". Supports dot and bracket notation for nested fields.
	Target string `json:"target"`

	// SkipQueryWhenTargetHasData skips the query when the target already holds
	// data. Default is false to ensure continuous reconciliation.
	// +optional
	SkipQueryWhenTargetHasData *bool `json:"skipQueryWhenTargetHasData,omitempty"`

	// QueryIntervalMinutes specifies the minimum interval between queries in
	// minutes, to prevent throttling. Default is 0 (no interval limiting).
	// +optional
	QueryIntervalMinutes *int `json:"queryIntervalMinutes,omitempty"`

	// Identity selects the authentication mechanism. Defaults to Secret.
	// +optional
	Identity *Identity `json:"identity,omitempty"`
}

// Filter is a generic name/values pair. See Input.Filters for per-QueryType
// interpretation.
type Filter struct {
	// Name of the filter (e.g. an EC2 filter name, a tag key, or a property).
	Name string `json:"name"`
	// Values to match. Semantics (OR within a filter) follow the target API.
	Values []string `json:"values"`
}

// Identity mirrors provider-upjet-aws spec.credentials + spec.assumeRoleChain so
// the same Secret used for the AWS provider can be reused with this function.
type Identity struct {
	// Source of the base credentials.
	// +kubebuilder:validation:Enum=Secret;IRSA;WebIdentity;PodIdentity
	// +kubebuilder:default=Secret
	// +optional
	Source IdentitySource `json:"source,omitempty"`

	// WebIdentity configures the AssumeRoleWithWebIdentity flow. Required when
	// Source is WebIdentity.
	// +optional
	WebIdentity *WebIdentity `json:"webIdentity,omitempty"`

	// AssumeRoleChain assumes a chain of roles (in order) on top of the base
	// credentials resolved from Source.
	// +optional
	AssumeRoleChain []AssumeRole `json:"assumeRoleChain,omitempty"`
}

// IdentitySource controls how base credentials are resolved.
// Supported values: Secret;IRSA;WebIdentity;PodIdentity.
type IdentitySource string

const (
	// IdentitySourceSecret uses a shared-credentials INI delivered via the
	// function credentials block named "aws-creds" (key "credentials").
	IdentitySourceSecret IdentitySource = "Secret"
	// IdentitySourceIRSA uses IAM Roles for Service Accounts: the function pod's
	// ServiceAccount carries the role; credentials come from the SDK default
	// chain (web identity token file). No secret required.
	IdentitySourceIRSA IdentitySource = "IRSA"
	// IdentitySourceWebIdentity uses an explicit role ARN plus a web identity
	// token (sourced from a Secret or the filesystem).
	IdentitySourceWebIdentity IdentitySource = "WebIdentity"
	// IdentitySourcePodIdentity uses EKS Pod Identity: credentials come from the
	// container credentials endpoint via the SDK default chain. No secret.
	IdentitySourcePodIdentity IdentitySource = "PodIdentity"
)

// WebIdentity configures the AssumeRoleWithWebIdentity flow.
type WebIdentity struct {
	// RoleARN is the IAM role to assume with the web identity token.
	RoleARN string `json:"roleARN"` //nolint:tagliatelle // mirrors provider-upjet-aws field name

	// RoleSessionName is an optional session name for the assumed role.
	// +optional
	RoleSessionName string `json:"roleSessionName,omitempty"`

	// TokenConfig selects where the web identity token comes from.
	// +optional
	TokenConfig *TokenConfig `json:"tokenConfig,omitempty"`
}

// TokenConfig selects the source of a web identity token.
type TokenConfig struct {
	// Source of the token. With Secret, the token bytes arrive via the function
	// credentials block (key "token"). With Filesystem, FSPath is read.
	// +kubebuilder:validation:Enum=Secret;Filesystem
	Source string `json:"source"`

	// FSPath is the path to the token file when Source is Filesystem.
	// +optional
	FSPath *string `json:"fsPath,omitempty"`
}

// AssumeRole describes a single STS AssumeRole hop.
type AssumeRole struct {
	// RoleARN is the role to assume.
	RoleARN string `json:"roleARN"` //nolint:tagliatelle // mirrors provider-upjet-aws field name

	// ExternalID to pass to AssumeRole, if the role's trust policy requires it.
	// +optional
	ExternalID *string `json:"externalID,omitempty"`

	// RoleSessionName for the assumed role session.
	// +optional
	RoleSessionName *string `json:"roleSessionName,omitempty"`

	// Tags are session tags to pass to AssumeRole.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// TransitiveTagKeys lists session tag keys that propagate to downstream
	// AssumeRole calls in the chain.
	// +optional
	TransitiveTagKeys []string `json:"transitiveTagKeys,omitempty"`
}
