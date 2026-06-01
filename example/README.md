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
  - name: us-east-1
    endpoint: ec2.us-east-1.amazonaws.com
    optInStatus: opt-in-not-required
  - name: eu-central-1
    endpoint: ec2.eu-central-1.amazonaws.com
    optInStatus: opt-in-not-required
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

## Dynamic queries

`region`, `filters`, and `parameters` can each be resolved at runtime from a
`status.`, `context.`, or `spec.` path via `regionRef`, `filtersRef`, and
`parametersRef` — so a prior pipeline step (or the XR itself) can drive the
query. This is the AWS analog of azresourcegraph's `queryRef`/`subscriptionsRef`.

`composition-dynamic-refs.yaml` pulls all three from the XR spec (see
`xr-dynamic.yaml`):

```shell
crossplane render xr-dynamic.yaml composition-dynamic-refs.yaml functions.yaml \
  --function-credentials=secrets/aws-creds.yaml -r
```

`composition-dynamic-context.yaml` instead pulls them from the pipeline
**context** — the chaining mechanism. In a real pipeline a prior step writes the
values to context; here `render` seeds them with `--context-values`:

```shell
crossplane render xr.yaml composition-dynamic-context.yaml functions.yaml \
  --function-credentials=secrets/aws-creds.yaml \
  --context-values='awsQuery={"region":"eu-central-1","imageParams":{"owners":"099720109477"},"imageFilters":[{"name":"name","values":["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]}]}' \
  -r
```

**Chaining queries:** to feed one query's output into another, write it to
`status.`/`context.` and reference it downstream. Reshaping between two AWS
queries (e.g. turning a caller-identity account ID into a filter) is typically
done with a `function-go-templating` / `function-python` step in between.

A `*Ref` overrides its static counterpart when set, and the referenced value
must match the field's shape: `filtersRef` → a list of `{name, values}` objects,
`parametersRef` → a string map, `regionRef` → a string. Reference paths traverse
maps (dot/bracket notation); list indexing (e.g. `[0]`) is not supported.

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

### Creating the Secret from `~/.aws/credentials`

The function reads the `[default]` profile of an AWS shared-credentials INI, so
your standard `~/.aws/credentials` works as-is — no editing or base64 by hand.

Apply it directly to a control plane (the name and namespace must match the
composition's `secretRef`):

```shell
kubectl -n crossplane-system create secret generic aws-creds \
  --from-file=credentials="$HOME/.aws/credentials"
```

Or generate `secrets/aws-creds.yaml` for `crossplane render` in one shot
(`--dry-run` writes the base64-encoded Secret that `--function-credentials`
expects):

```shell
kubectl create secret generic aws-creds -n crossplane-system \
  --from-file=credentials="$HOME/.aws/credentials" \
  --dry-run=client -o yaml > secrets/aws-creds.yaml
```

Using a named profile? Export it as a `[default]` file first, then point
`--from-file` at that:

```shell
aws configure export-credentials --profile myprofile --format process \
  | jq -r '"[default]\naws_access_key_id = \(.AccessKeyId)\naws_secret_access_key = \(.SecretAccessKey)" + (if .SessionToken then "\naws_session_token = \(.SessionToken)" else "" end)' \
  > /tmp/aws-creds
```
