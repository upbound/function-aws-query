import models.io.upbound.dev.meta.v1alpha1 as metav1alpha1

# End-to-end test for function-aws-query. `up test run tests/* --e2e` installs
# this on a Spaces control plane and waits for the XR to go Ready.
#
# The function package under test: CI (.github/workflows/e2e.yaml) replaces the
# @FUNCTION_PKG@ sentinel with the package built and pushed from the PR head.
_functionPackage = "@FUNCTION_PKG@"

_items = [
    metav1alpha1.E2ETest{
        metadata.name: "e2e-aws-query"
        spec = {
            crossplane.autoUpgrade.channel: "Rapid"
            # Succeeds only if both queries ran and function-auto-ready marked the
            # XR Ready. A failed AWS call makes the function return fatal, the XR
            # never goes Ready, and the test fails -- the end-to-end signal.
            defaultConditions: ["Ready"]
            initResources: [
                # proidc injects a web-identity token into the function pod, keyed
                # off this annotation -- the same mechanism providers get via
                # credentials.source: Upbound. The composition reads the token and
                # assumes the solutions-e2e-function AWS role.
                {
                    apiVersion: "pkg.crossplane.io/v1beta1"
                    kind: "DeploymentRuntimeConfig"
                    metadata.name: "function-aws-query-e2e"
                    spec.deploymentTemplate.spec = {
                        selector: {}
                        template.metadata.annotations: {
                            "proidc.cloud-spaces.upbound.io/identity" = "function.function-aws-query"
                        }
                        # The pod template requires at least one container; name it
                        # so the annotation merges onto the function's runtime pod
                        # rather than adding a second container.
                        template.spec.containers: [
                            {
                                name = "package-runtime"
                            }
                        ]
                    }
                }
                # The function under test, with the runtime config attached.
                # Installed here (not via the project's dependsOn) because dependsOn
                # cannot set a runtimeConfigRef.
                {
                    apiVersion: "pkg.crossplane.io/v1"
                    kind: "Function"
                    metadata.name: "function-aws-query"
                    spec = {
                        package: _functionPackage
                        runtimeConfigRef.name: "function-aws-query-e2e"
                    }
                }
            ]
            manifests: [
                {
                    apiVersion: "e2e.aws-query.upbound.io/v1alpha1"
                    kind: "XQuery"
                    metadata = {
                        name: "e2e-query"
                        namespace: "default"
                    }
                    spec.region: "eu-central-1"
                }
            ]
            skipDelete: False
            timeoutSeconds: 900
        }
    }
]
items = _items
