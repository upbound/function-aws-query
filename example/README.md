# Example manifests

Run the function locally and exercise it with `crossplane render`. The query
backends make real AWS calls, so supply working credentials via
`--function-credentials` (or use IRSA/PodIdentity when running in-cluster).

```shell
# Run the function locally
go run . --insecure --debug
```

```shell
# In another terminal, render an example. Edit secrets/aws-creds.yaml first.
crossplane render xr.yaml composition.yaml functions.yaml \
  --function-credentials=secrets/aws-creds.yaml -r
```

The result is written to the XR `status` (or the pipeline `context`), e.g. for
`composition.yaml`:

```yaml
apiVersion: example.io/v1alpha1
kind: XAccount
metadata:
  name: example-account
status:
  allAvailableRegions:
  - {name: us-east-1, endpoint: ec2.us-east-1.amazonaws.com, optInStatus: opt-in-not-required}
  - {name: eu-central-1, endpoint: ec2.eu-central-1.amazonaws.com, optInStatus: opt-in-not-required}
  # ...
```

## Per-queryType examples

| Composition | queryType | Backend | Target |
|---|---|---|---|
| `composition.yaml` | DescribeRegions | EC2 | `status.allAvailableRegions` |
| `composition-caller-identity.yaml` | GetCallerIdentity | STS | `status.callerIdentity` |
| `composition-availability-zones.yaml` | DescribeAvailabilityZones | EC2 | `status.availabilityZones` |
| `composition-ami-lookup.yaml` | DescribeImages | EC2 | `status.amis` |
| `composition-service-quotas.yaml` | ListServiceQuotas | Service Quotas | `status.ec2Quotas` |
| `composition-cloudcontrol-vpcs.yaml` | ListResources | Cloud Control | `status.prodVpcs` |
| `composition-tagging-subnets.yaml` | GetResources | Resource Groups Tagging API | `context.subnets` |

```shell
crossplane render xr.yaml composition-caller-identity.yaml functions.yaml \
  --function-credentials=secrets/aws-creds.yaml -r
```

## Authentication

The credentials Secret is **byte-identical to provider-upjet-aws** — reuse your
existing AWS provider secret. The `Identity.source` selects the mechanism:

| `identity.source` | Credentials block | Notes |
|---|---|---|
| `Secret` (default) | `secrets/aws-creds.yaml` (shared-creds INI under `credentials`) | Long-term keys, like the provider Secret source. |
| `IRSA` | none | Function pod ServiceAccount carries the role — see `deploymentruntimeconfig.yaml`. |
| `PodIdentity` | none | EKS Pod Identity association for the function's ServiceAccount. |
| `WebIdentity` | `secrets/web-identity-token.yaml` (OIDC token under `token`) | Explicit `roleARN` + token (Secret or filesystem). |

An optional `identity.assumeRoleChain` assumes one or more roles on top of the
base credentials (e.g. cross-account reads). See `composition-irsa.yaml`.
