# function-aws-query

A [composition function][functions] that runs **read-only** queries against AWS
APIs and exposes the results as structured data to downstream composition steps
(function-python, function-kcl, go-templating, patch-and-transform, etc.) —
available regions, AZs, AMI lookups, caller identity, service quotas, and
existing resources by tag. It is the AWS analog of
[function-azresourcegraph][azrg] (Azure) and [function-msgraph][msgraph]
(Microsoft Graph).

## What it can query

Each composition pipeline step supplies an `Input` selecting a `queryType`. The
result is written to `target` (`status.<field>` or `context.<field>`).

**Service-metadata** (direct EC2/STS/Service Quotas calls — no account setup):

| `queryType` | Returns |
|---|---|
| `GetCallerIdentity` | `{account, arn, userId}` |
| `DescribeRegions` | `[{name, endpoint, optInStatus}]` |
| `DescribeAvailabilityZones` | `[{name, zoneId, state, regionName, zoneType, groupName}]` |
| `DescribeImages` | `[{imageId, name, ownerId, creationDate, architecture, state, rootDeviceType, description}]` |
| `DescribeRouteTables` | `[{routeTableId, vpcId, ownerId, associations[{routeTableAssociationId, routeTableId, subnetId, gatewayId, main, state}], routes[{destinationCidrBlock, destinationIpv6CidrBlock, destinationPrefixListId, gatewayId, natGatewayId, transitGatewayId, vpcPeeringConnectionId, egressOnlyInternetGatewayId, carrierGatewayId, localGatewayId, coreNetworkArn, instanceId, networkInterfaceId, origin, state}], tags{}}]`. Carries the **main association ID**, which no CloudFormation schema models. `filters` **required**: `vpc-id`, `route-table-id`, `association.subnet-id`, `tag:<key>`. |
| `DescribeSubnets` | `[{subnetId, subnetArn, vpcId, ownerId, availabilityZone, availabilityZoneId, cidrBlock, state, defaultForAz, mapPublicIpOnLaunch, availableIpAddressCount, ipv6Native, ipv6CidrBlockAssociationSet[{associationId, ipv6CidrBlock, state}], tags{}}]`. Returns only **live** subnets, unlike the Tagging API. `filters` **required**: `vpc-id`, `subnet-id`, `availability-zone`, `tag:<key>`. |
| `DescribeSecurityGroupRules` | `[{securityGroupRuleId, securityGroupRuleArn, groupId, groupOwnerId, isEgress, ipProtocol, fromPort, toPort, cidrIpv4, cidrIpv6, prefixListId, referencedGroupId, referencedGroupUserId, referencedGroupVpcId, description, tags{}}]`. `filters` **required**: `group-id`, `security-group-rule-id`, `tag:<key>` - this operation does **not** accept `vpc-id`. |
| `ListServiceQuotas` | `[{quotaCode, quotaName, value, unit, adjustable, globalQuota}]` (all quotas for a `serviceCode`) |
| `GetServiceQuota` | `{quotaCode, quotaName, value, unit, adjustable, globalQuota}` (a single quota; needs `serviceCode`+`quotaCode`) |

**Existing-resource inventory** (generic, no Config/Resource-Explorer setup):

| `queryType` | Backend | Returns |
|---|---|---|
| `ListResources` | AWS Cloud Control | `[{identifier, properties{…}}]` for any CloudFormation-modeled type; each resource is hydrated via `GetResource` for full attributes + tags (set `parameters.hydrate=false` for identifiers only). Filter client-side via `filters`. |
| `GetResources` | Resource Groups Tagging API | `[{arn, tags{}}]` — server-side tag/type filtering |

### Which one to use?

- **`GetResources` (Tagging API)** — when you just need to **find resources by
  tag** and the ARN/ID is enough. It's cheap: one IAM permission
  (`tag:GetResources`), server-side tag filtering, one call across many services.
  Caveats: returns only ARN + tags (no attributes), skips never-tagged resources,
  and is eventually consistent.
- **`ListResources` (Cloud Control)** — when you need the resource's **full
  attributes** (CIDR, AZ, state, …), or want to enumerate **all** resources of a
  type including untagged ones. Caveats: needs that type's read IAM permissions,
  filters are applied client-side, and hydration costs one `GetResource` call per
  resource (use `hydrate=false` to skip it when you only want identifiers).
- **`DescribeRouteTables` / `DescribeSubnets` / `DescribeSecurityGroupRules`** -
  when the identifier you need is not in the resource's CloudFormation schema (a
  route table's **main association ID** is the canonical case), or when you need
  an **authoritative** answer. Needs `ec2:DescribeRouteTables`,
  `ec2:DescribeSubnets` and `ec2:DescribeSecurityGroupRules` respectively;
  without them the call fails at reconcile with `UnauthorizedOperation`. It
  reads only what the `filters` select, server-side, so a foreign resource cannot
  fail the query. `filters` are **required**, and the accepted names differ per
  query type (see the table above); unfiltered these would be region-wide reads.
  Filter *values* are not validated - an id that does not exist yields an empty
  result, not an error - but an unrecognised filter *name* is fatal. Both
  alternatives can mislead here:
  Cloud Control's `ListResources` walks every resource of the type account-wide
  and aborts the whole composition if any one of them fails to hydrate, and the
  Tagging API can keep reporting deleted resources for a while - which, if they
  carry the same identifying tag as their live replacements, silently doubles
  the result set.

Rule of thumb: *IDs by tag →* `GetResources`; *attributes / full inventory of a
type →* `ListResources`; *EC2 identifiers CloudFormation does not model, or an
authoritative VPC-scoped read →* the direct `Describe*` query types.

On absent values, the direct EC2 describes take two deliberate policies. Fields
AWS always returns are projected with their zero value (`isEgress: false`,
`main: false`, `cidrBlock: ""`). Fields that are genuinely optional are
**omitted** rather than faked: `fromPort`/`toPort` are absent on a rule that has
no ports, and `referencedGroup*` only appears on a group-referencing rule.
Omitting is the honest choice - `fromPort: 0` is a real port - so guard in
templates (`{{ if .fromPort }}`) rather than assuming the key exists. Note real
EC2 does send `fromPort: -1, toPort: -1` for an all-protocol rule, so a truly
absent port is rarer than it looks.

## Input reference

```yaml
apiVersion: aws.fn.crossplane.io/v1beta1
kind: Input
queryType: <one of the above>          # required
region: eu-central-1                    # or regionRef: spec.region (status./context./spec.)
filters:                                # or filtersRef: <path> (status./context./spec.)
- name: "tag:Environment"
  values: ["prod"]
parameters:                             # or parametersRef: <path>; scalar args per queryType:
  # allRegions, allAvailabilityZones (bool); owners, imageIds (csv)        [EC2]
  # serviceCode, quotaCode                                                 [Service Quotas]
  # typeName, resourceModel, roleArn, hydrate (default true)               [Cloud Control]
  # resourceTypeFilters (csv)                                              [Tagging API]
  typeName: "AWS::EC2::VPC"
target: status.prodVpcs                 # required; status.* or context.*
skipQueryWhenTargetHasData: true        # optional
queryIntervalMinutes: 10                # optional throttle
identity:                               # optional; defaults to Secret
  source: Secret                        # Secret | IRSA | WebIdentity | PodIdentity
  assumeRoleChain:
  - roleARN: arn:aws:iam::222222222222:role/crossplane-readonly
```

**Dynamic queries:** `regionRef`, `filtersRef`, and `parametersRef` resolve their
value from a `status.`/`context.`/`spec.` path at runtime (overriding the static
field), so a prior pipeline step or the XR can drive the query — the AWS analog
of azresourcegraph's `queryRef`/`subscriptionsRef`.

## Authentication

Mirrors `provider-upjet-aws` — the credentials Secret is a shared-credentials
INI **byte-identical to the AWS provider's Secret source**, so existing secrets
are reusable. Supported `identity.source` values: `Secret` (default), `IRSA`,
`WebIdentity` (token via Secret or filesystem), `PodIdentity`, plus an
`assumeRoleChain`. See [`example/README.md`](example/README.md) for the full
matrix and a `DeploymentRuntimeConfig` for IRSA/PodIdentity.

## Usage

### Quick check with `crossplane render` (no cluster)

Validates a composition against **real AWS** in seconds — the fastest way to try
a query.

```shell
# 1. Put working AWS credentials in example/secrets/aws-creds.yaml: base64 of an
#    AWS shared-credentials INI, under the "credentials" key. For example:
#      printf '[default]\naws_access_key_id = AKIA...\naws_secret_access_key = ...\n' | base64

# 2. Run the function locally.
go run . --insecure --debug &

# 3. Render any example (see example/ for one composition per queryType).
crossplane render example/xr.yaml example/composition.yaml example/functions.yaml \
  --function-credentials=example/secrets/aws-creds.yaml -rc
```

The rendered XR shows the query result under `status` (here,
`status.allAvailableRegions`).

### Run it on a control plane (end-to-end)

```shell
# 1. Install the function. Replace the tag with a released version (or your own
#    pushed package).
kubectl apply -f - <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: function-aws-query
spec:
  package: xpkg.upbound.io/upbound/function-aws-query:v0.1.0
EOF

# 2. Create the credentials Secret in your control plane's system namespace.
#    It must match the secretRef in the composition (name + namespace). Reuse the
#    same secret you already use for provider-aws, or create one from a file:
kubectl -n crossplane-system create secret generic aws-creds \
  --from-file=credentials="$HOME/.aws/credentials"

# 3. Apply the API definition, the composition, and an example resource.
kubectl apply -f example/definition.yaml
kubectl apply -f example/composition.yaml
kubectl apply -f example/xr.yaml

# 4. Confirm the AWS query result lands on the resource's status.
kubectl get xaccount example-account -o yaml
```

For IRSA / Pod Identity instead of a Secret, attach a `DeploymentRuntimeConfig`
to the Function (see [`example/deploymentruntimeconfig.yaml`](example/deploymentruntimeconfig.yaml))
and set `identity.source: IRSA` (or `PodIdentity`) — no Secret needed.

## Develop

```shell
# Run code generation - see input/generate.go
go generate ./...

# Run tests - see fn_test.go and aws_test.go
go test -v ./...

# Lint
golangci-lint run

# Build the function's runtime image and package
docker build . --tag=runtime
crossplane xpkg build -f package --embed-runtime-image=runtime
```

See [`example/README.md`](example/README.md) for the full per-`queryType` render
matrix and the authentication-source reference.

[functions]: https://docs.crossplane.io/latest/concepts/composition-functions
[azrg]: https://github.com/upbound/function-azresourcegraph
[msgraph]: https://github.com/upbound/function-msgraph
