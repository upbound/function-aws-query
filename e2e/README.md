# function-aws-query end-to-end tests

This directory is a self-contained Upbound project used only to run **real**
end-to-end tests for `function-aws-query`. It installs the function on an
ephemeral control plane and proves it answers live AWS queries — the function
analog of the `up test run tests/* --e2e` harness the solutions
`configuration-*` repos use.

It is never published to the Marketplace; `up test` builds it and loads it into a
throwaway control plane.

## Layout

```
e2e/
├── upbound.yaml                 # test configuration; depends on the function under test (pinned by CI)
├── apis/xqueries/               # XRD + Composition that call the function
├── functions/.gitkeep           # must exist for `up`
└── tests/
    └── e2etest-queries/
        └── test.yaml            # E2ETest: patches the default DRC, applies an XQuery, waits for Ready
```

`up test` builds this project, installs it on the control plane (the function
under test and `function-auto-ready` come along as dependencies), then creates the
XQuery from `test.yaml`. Dependency-installed functions are named `<org>-<repo>`,
so the Composition references `solutions-function-aws-query` and
`crossplane-contrib-function-auto-ready`.

The two query types (`GetCallerIdentity`, `DescribeRegions`) are deliberately the
cheapest: they need no pre-provisioned AWS resources, create nothing, and need
only a valid identity (plus `ec2:DescribeRegions`). A real failure — bad
credentials, a broken query path, a function regression — makes the function
return a fatal result, so the XR never becomes `Ready` and the test fails.

## Credentials (no AWS secrets in CI)

The function gets its AWS identity in-cluster, the same way the solutions
`configuration-*` repos do. Upbound's `proidc` mints a web-identity token for the
function pod and the function assumes an AWS role with it:

- `test.yaml` patches the control plane's **`default`** `DeploymentRuntimeConfig`
  with the annotation `proidc.cloud-spaces.upbound.io/audience: sts.amazonaws.com`.
  That's the only field the Spaces DRC policy allows (plus `--debug`, `--poll`,
  0/1 replicas and package-runtime env vars); `proidc` **auto-assigns** the
  workload identity (`function.function-aws-query`) — you must not set it yourself.
- `proidc` then mounts the token at **`/var/run/secrets/upbound.io/provider/token`**
  (the same path for function pods as for provider pods), and the Composition's
  `identity.source: WebIdentity` reads it with the dedicated function `roleARN`.

The only value substituted at run time is `@FUNCTION_VERSION@` in `upbound.yaml`
(the dev tag built from the PR), never credentials.

### AWS setup (one dedicated role — do NOT widen the shared provider role)

Role `solutions-e2e-function`, trust policy below. It differs from
`solutions-e2e-provider-aws` only in the subject (`:function:*` instead of
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

Attach a minimal read policy — for this harness only `ec2:DescribeRegions`
(`GetCallerIdentity` needs no permission).

## Two repos, one function

The function dev build is pushed to `solutions/<repo>` (the function's own repo),
and the wrapper project to **`solutions/<repo>-e2e-wrapper`**. They must be separate:
Crossplane's package lock keys dependency-graph nodes by source repo, so a project
and a function sharing one repo collide (`node ... already exists`). The
`<repo>-e2e-wrapper` repo is granted to the e2e robot in `solutions-gitops-prod`.

## What runs in CI

On a pull request labelled `run-e2e-tests` (Renovate adds this automatically),
`.github/workflows/e2e.yaml`:

1. builds the function package from the PR head and pushes it to `solutions/<repo>`
   at a dev tag,
2. pins that tag in `upbound.yaml` (`@FUNCTION_VERSION@`),
3. switches to the e2e Spaces control plane group, and
4. runs `up test run tests/* --e2e`, pushing the wrapper project to
   `solutions/<repo>-e2e-wrapper` (no AWS credentials involved).

Branch protection on `main` requires the `e2e` check, so a PR — including a
Renovate security-patch PR — can only auto-merge once this passes.

## Fast local iteration (no control plane)

For quick checks against real AWS without a control plane, use the render harness
in [`../example`](../example) (`make -C ../example render`). The e2e harness here
is the heavier, control-plane-backed gate.

<!-- throwaway: e2e smoke test of the shipped workflow, do not merge -->
