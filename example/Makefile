# Render every function-aws-query example against a locally running instance of
# the function, using `crossplane render`.
#
#   make render        # build+start the function, render every example, report PASS/FAIL, stop
#   make commands      # print the copy-pasteable render commands (for manual inspection)
#   make serve         # run the function in the foreground (for manual `crossplane render`)
#   make render-irsa   # render the IRSA example (needs real IRSA/role; excluded from `render`)
#   make clean
#
# Requires: go, the crossplane CLI, and working AWS credentials in
# secrets/aws-creds.yaml (the committed file is a placeholder).

SHELL := /bin/bash
.ONESHELL:
.DEFAULT_GOAL := render

ROOT  := ..
BIN   ?= /tmp/function-aws-query-render
ADDR  ?= localhost:9443
PORT  := $(lastword $(subst :, ,$(ADDR)))
CREDS := secrets/aws-creds.yaml
FUNCS := functions.yaml

# Context payload for the context-sourced dynamic example.
CTX := awsQuery={"region":"eu-central-1","imageParams":{"owners":"099720109477"},"imageFilters":[{"name":"name","values":["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]}]}

# Single source of truth: name|xr|composition|extra-args. Both `render` and
# `commands` iterate over this, so the executed and printed commands never drift.
define EXAMPLES
regions|xr.yaml|composition.yaml|
caller-identity|xr.yaml|composition-caller-identity.yaml|
availability-zones|xr.yaml|composition-availability-zones.yaml|
ami-lookup|xr.yaml|composition-ami-lookup.yaml|
service-quotas|xr.yaml|composition-service-quotas.yaml|
cloudcontrol-vpcs|xr.yaml|composition-cloudcontrol-vpcs.yaml|
tagging-subnets|xr.yaml|composition-tagging-subnets.yaml|
dynamic-refs|xr-dynamic.yaml|composition-dynamic-refs.yaml|
dynamic-context|xr.yaml|composition-dynamic-context.yaml|--context-values='$(CTX)'
endef
export EXAMPLES

.PHONY: render commands serve render-irsa build clean help

build: $(BIN)

$(BIN): $(wildcard $(ROOT)/*.go) $(wildcard $(ROOT)/input/v1beta1/*.go)
	@echo ">> building function"
	cd $(ROOT) && go build -o $(BIN) .

serve: build
	@echo ">> serving on $(ADDR) (Ctrl-C to stop)"
	$(BIN) --insecure --debug --address=$(ADDR)

# Print the render commands for manual copy-paste. Start the function first
# (`make serve` in another terminal); these connect to it via the Development
# runtime. Add `-o yaml` or pipe to `yq`/`less` to inspect the full output.
commands:
	@echo "# Start the function first (another terminal):  make serve"
	@echo "# Then run any of these from the example/ directory:"
	@echo
	@while IFS='|' read -r name xr comp extra; do \
	  [ -z "$$name" ] && continue; \
	  echo "# $$name"; \
	  echo "crossplane render $$xr $$comp $(FUNCS) --function-credentials=$(CREDS) $$extra -r"; \
	  echo; \
	done <<< "$$EXAMPLES"
	@echo "# irsa (needs real IRSA/Pod Identity + a valid assumeRoleChain role)"
	@echo "crossplane render xr.yaml composition-irsa.yaml $(FUNCS) -r"

render: build
	@set -uo pipefail
	creds=$$(grep 'credentials:' $(CREDS) | awk '{print $$2}' | base64 -d 2>/dev/null)
	if printf '%s' "$$creds" | grep -q REPLACE_WITH; then
	  echo "secrets/aws-creds.yaml is a placeholder — put real AWS credentials there first."
	  echo "(it is skip-worktree'd, so your changes stay out of git)"
	  exit 2
	fi
	$(BIN) --insecure --address=$(ADDR) >/tmp/fn-render.log 2>&1 &
	FN_PID=$$!
	trap 'kill $$FN_PID 2>/dev/null || true' EXIT
	for i in $$(seq 1 40); do (exec 3<>/dev/tcp/127.0.0.1/$(PORT)) 2>/dev/null && break; sleep 0.25; done
	fail=0
	while IFS='|' read -r name xr comp extra; do
	  [ -z "$$name" ] && continue
	  if eval "crossplane render $$xr $$comp $(FUNCS) --function-credentials=$(CREDS) $$extra -r" >/tmp/render-$$name.out 2>&1; then
	    echo "PASS  $$name"
	  else
	    echo "FAIL  $$name"; tail -2 /tmp/render-$$name.out | sed 's/^/      /'; fail=1
	  fi
	done <<< "$$EXAMPLES"
	echo
	if [ $$fail -ne 0 ]; then echo "Some examples failed (see /tmp/render-*.out)"; exit 1; fi
	echo "All examples rendered successfully."

# The IRSA example uses identity.source: IRSA with a placeholder assumeRoleChain,
# so it only succeeds with real IRSA/Pod Identity and a valid role. Excluded from
# `render`; run explicitly to exercise the auth path.
render-irsa: build
	@echo ">> IRSA example: needs real IRSA/Pod Identity + a valid assumeRoleChain role"
	$(BIN) --insecure --address=$(ADDR) >/tmp/fn-render.log 2>&1 &
	FN_PID=$$!
	trap 'kill $$FN_PID 2>/dev/null || true' EXIT
	for i in $$(seq 1 40); do (exec 3<>/dev/tcp/127.0.0.1/$(PORT)) 2>/dev/null && break; sleep 0.25; done
	crossplane render xr.yaml composition-irsa.yaml $(FUNCS) -r

clean:
	rm -f $(BIN) /tmp/render-*.out /tmp/fn-render.log

help:
	@echo "targets:"
	@echo "  render       build+start the function, render every example, report PASS/FAIL (default)"
	@echo "  commands     print the copy-pasteable render commands for manual inspection"
	@echo "  serve        run the function in the foreground for manual 'crossplane render'"
	@echo "  render-irsa  render the IRSA example (needs real IRSA/role)"
	@echo "  build        build the function binary ($(BIN))"
	@echo "  clean        remove the built binary and render logs"
