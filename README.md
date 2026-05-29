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
| `ListServiceQuotas` / `GetServiceQuota` | `[{quotaCode, quotaName, value, unit, adjustable, globalQuota}]` |

**Existing-resource inventory** (generic, no Config/Resource-Explorer setup):

| `queryType` | Backend | Returns |
|---|---|---|
| `ListResources` | AWS Cloud Control | `[{identifier, properties{…}}]` — full attributes for any CloudFormation-modeled type; filter client-side via `filters` |
| `GetResources` | Resource Groups Tagging API | `[{arn, tags{}}]` — server-side tag/type filtering |

## Input reference

```yaml
apiVersion: aws.fn.crossplane.io/v1beta1
kind: Input
queryType: <one of the above>          # required
region: eu-central-1                    # or regionRef: spec.region (status./context./spec.)
filters:                                # EC2 filter names | tag filters | client-side match
- name: "tag:Environment"
  values: ["prod"]
parameters:                             # scalar args per queryType:
  # allRegions, allAvailabilityZones (bool); owners, imageIds (csv)        [EC2]
  # serviceCode, quotaCode                                                 [Service Quotas]
  # typeName, resourceModel, roleArn                                       [Cloud Control]
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
  --function-credentials=example/secrets/aws-creds.yaml -r
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
kubectl -n upbound-system create secret generic aws-creds \
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
