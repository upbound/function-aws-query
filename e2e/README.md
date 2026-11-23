# function-aws-query end-to-end tests

This directory is a self-contained Upbound project used only to run **real**
end-to-end tests for `function-aws-query`. It installs the function on an
ephemeral control plane and proves it answers live AWS queries — the function
analog of the `up test run tests/* --e2e` harness the solutions
`configuration-*` repos use.

It is never published to the Marketplace; `up test` builds it locally and loads
it into a throwaway control plane.

## Layout

```
e2e/
├── upbound.yaml                      # throwaway project; dependsOn function-aws-query (+ function-auto-ready)
├── apis/
│   ├── definition.yaml               # XRD: XQuery (namespaced, Crossplane v2)
│   └── composition.yaml              # pipeline: GetCallerIdentity + DescribeRegions + auto-ready
└── tests/
    └── e2etest-queries/
        └── e2etest.yaml              # E2ETest: installs the function + token projection, applies an XQuery, waits for Ready
```

The two query types (`GetCallerIdentity`, `DescribeRegions`) are deliberately
the cheapest ones: they need no pre-provisioned AWS resources, create nothing,
and need only a valid identity (plus `ec2:DescribeRegions`). A real failure —
bad credentials, a broken query path, a function regression — makes the function
return a fatal result, so the XR never becomes `Ready` and the test fails.

## Credentials

The function needs an AWS identity at runtime, and it uses the **same in-cluster
method as the solutions `configuration-*` repos — no AWS secrets in CI**. Upbound's
`proidc` system mints a web-identity token for each annotated workload; the
function assumes an AWS role with it:

- `tests/e2etest-queries/e2etest.yaml` installs a `DeploymentRuntimeConfig` that
  annotates the function pod with
  `proidc.cloud-spaces.upbound.io/identity: function.function-aws-query`. `proidc`
  then injects a token (issuer `proidc.upbound.io`, subject
  `mcp:<org>/<cp>:function:function-aws-query`, audience `sts.amazonaws.com`) and
  mounts it. Providers get the equivalent via `credentials.source: Upbound`; the
  function has no `Upbound` source, so it reads the token directly.
- `apis/composition.yaml` uses `identity.source: WebIdentity` pointed at the
  injected token (`fsPath`) and the dedicated function e2e `roleARN`.

The only value substituted at run time is `${FUNCTION_PKG}` (the package built
from the PR), not credentials.

### AWS setup (one dedicated role — do NOT widen the shared provider role)

Create a role, e.g. `solutions-e2e-function`, with this trust policy. It differs
from `solutions-e2e-provider-aws` only in the subject (`:function:*` instead of
`:provider:provider-aws`), so one role serves every function repo:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::609897127049:oidc-provider/proidc.upbound.io" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": { "StringLike": {
      "proidc.upbound.io:sub": "mcp:solutions/*:function:*",
      "proidc.upbound.io:aud": "sts.amazonaws.com"
    }}
  }]
}
```

Attach a minimal read policy — for this harness only `ec2:DescribeRegions` is
needed (`GetCallerIdentity` needs no permission).

> **Two things to confirm on the first live run.** (1) that `proidc` mints an
> identity for the `function.*` kind (it does for `provider.*`; the subject scheme
> implies functions are supported but it is unproven), and (2) the exact mount
> path of the injected token for a function pod (providers get
> `/var/run/secrets/upbound.io/provider/token`; adjust `fsPath` if the function
> kind differs).

## What runs in CI

On a pull request labelled `run-e2e-tests` (Renovate adds this label
automatically), `.github/workflows/e2e.yaml`:

1. builds the function package from the PR head and pushes it to a dev tag,
2. substitutes that package into the `E2ETest` (`${FUNCTION_PKG}`),
3. switches to the e2e Spaces control plane group, and
4. runs `up test run tests/* --e2e` (no AWS credentials involved).

Branch protection on `main` requires the `e2e` check, so a PR — including a
Renovate security-patch PR — can only auto-merge once this passes.

## Fast local iteration (no control plane)

For quick checks against real AWS without a control plane, use the render
harness in [`../example`](../example) (`make -C ../example render`), which runs
every example composition through `crossplane render`. The e2e harness here is
the heavier, control-plane-backed gate.

<!-- e2e smoke test: triggers the first live run of the e2e harness. -->
