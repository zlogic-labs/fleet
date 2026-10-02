# Fleet build entry points.
#
# Fleet is pure Go: no cgo, no C toolchain, no architecture-specific code. Every
# release binary below is statically linked and cross-compiles from any host.
# If a target ever needs CGO_ENABLED=1, something has taken a native dependency
# and should not have.

CGO_ENABLED ?= 0
GOBIN       := $(CURDIR)/bin
# core holds the two processes an installation runs: the gateway that serves
# the OpenAI API, and the control plane. The Kubernetes controller lives in its
# own repository (zlogic-labs/fleet-serving) and its own module, so nothing in
# core can reach for client-go by accident.
CORE_CMDS   := fleet-gateway fleet-apiserver
MODS        := core
PKGS        := $(MODS:%=%/...)

# The console is an npm project whose output is embedded into
# core/internal/gateway/webui/dist. Building it here rather than in a separate
# repository means `make build` can embed a current console with no second
# checkout to pin.
WEB_DIR    := web
WEB_OUT    := $(WEB_DIR)/dist
WEB_DIST   := core/internal/gateway/webui/dist

# Release targets. linux/amd64 covers NVIDIA and the mainstream domestic GPU
# cards; linux/arm64 covers Ascend 910B on Kunpeng and Apple Silicon hosts;
# darwin/arm64 is for local development.
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/arm64

.DEFAULT_GOAL := build

.PHONY: web-deps
web-deps: ## Install the console's npm dependencies
	cd $(WEB_DIR) && npm ci --no-audit --no-fund

.PHONY: web
web: web-deps ## Build the console and stage it for embedding into the gateway
	cd $(WEB_DIR) && npm run build
	@rm -rf $(WEB_DIST) && mkdir -p $(WEB_DIST) && touch $(WEB_DIST)/.gitkeep
	cp -R $(WEB_OUT)/. $(WEB_DIST)/
	@echo "  staged $$(find $(WEB_DIST) -type f ! -name .gitkeep | wc -l) files into $(WEB_DIST)"

.PHONY: web-dev
web-dev: web-deps ## Run the console dev server against a local gateway
	cd $(WEB_DIR) && npm run dev

# Running the whole product locally, not just building it. dev.sh is the
# primary entry point and works without make; these targets exist so the common
# sequences are discoverable.
.PHONY: dev
dev: ## Start the gateway and the control plane with a synthetic model hub
	./scripts/dev.sh

.PHONY: dev-real
dev-real: ## Same, but pulls come from huggingface.co
	./scripts/dev.sh --real

.PHONY: seed
seed: ## Fill the dev control plane with models, pulls and a cluster report
	./scripts/seed.sh

# End-to-end assertions against a running stack. Separate from `test`, which
# runs without a server, because this one needs both processes up and therefore
# cannot be part of `make check`.
.PHONY: smoke
smoke: ## Assert end-to-end behaviour against a running ./scripts/dev.sh
	./scripts/smoke.sh

.PHONY: engine-image
engine-image: ## Stage a real llama.cpp image and a real quantized model into k3s
	./scripts/k3s-engine.sh

.PHONY: e2e
e2e: ## Pull, deploy to k3s, and infer through the gateway
	FLEET_SERVING_DIR=$${FLEET_SERVING_DIR:-$$(cd .. && pwd)/fleet-serving} ./scripts/e2e.sh

.PHONY: build
build: ## Compile every command for the host platform into ./bin
	@mkdir -p $(GOBIN)
	@for cmd in $(CORE_CMDS); do \
		echo "  build $$cmd"; \
		CGO_ENABLED=$(CGO_ENABLED) go build -o $(GOBIN)/$$cmd ./core/cmd/$$cmd || exit 1; \
	done

.PHONY: build-release
build-release: web ## Cross-compile for every supported platform
	@for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		for cmd in $(CORE_CMDS); do \
			echo "  build $$cmd for $$target"; \
			CGO_ENABLED=$(CGO_ENABLED) GOOS=$$os GOARCH=$$arch \
				go build -o $(GOBIN)/$$os-$$arch/$$cmd ./core/cmd/$$cmd || exit 1; \
		done; \
	done

.PHONY: test
test: ## Run the unit tests
	CGO_ENABLED=$(CGO_ENABLED) go test $(PKGS)

.PHONY: test-race
test-race: ## Run the unit tests under the race detector (needs a C toolchain)
	CGO_ENABLED=1 go test -race $(PKGS)

.PHONY: cover
cover: ## Report coverage per package
	CGO_ENABLED=$(CGO_ENABLED) go test -coverprofile=coverage.out $(PKGS)
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	CGO_ENABLED=$(CGO_ENABLED) go vet $(PKGS)

.PHONY: check-cgo
check-cgo: ## Fail if anything in the dependency graph needs a C toolchain
	@found=$$(go list -deps -f '{{if .CgoFiles}}{{.ImportPath}}{{end}}' $(PKGS) | grep -v '^$$'); \
	if [ -n "$$found" ]; then \
		echo "cgo dependencies found:"; echo "$$found"; exit 1; \
	fi; \
	echo "  no cgo dependencies"

.PHONY: fmt
fmt: ## Format every package
	gofmt -w $(MODS)

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l $(MODS)); \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## Tidy every module
	@for m in $(MODS); do \
		echo "  tidy $$m"; \
		(cd $$m && go mod tidy) || exit 1; \
	done

.PHONY: check
check: fmt-check vet check-cgo test web-typecheck ## Everything CI runs

.PHONY: web-typecheck
web-typecheck: ## Typecheck the console
	cd $(WEB_DIR) && npx tsc -b --noEmit

.PHONY: clean
clean:
	rm -rf $(GOBIN) coverage.out $(WEB_OUT)

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
