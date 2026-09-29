# Fleet build entry points.
#
# GOARCH is pinned because the local toolchain is a 32-bit Windows build; a
# gateway that holds thousands of streaming connections needs the full address
# space. Remove the override once a 64-bit toolchain is installed.

GOARCH ?= amd64
GOBIN  := $(CURDIR)/bin
CMDS   := fleet-gateway fleet-apiserver
# Only the modules that exist right now; operator/ is added when it lands.
MODS   := $(wildcard core operator)
PKGS   := $(MODS:%=%/...)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Compile every command into ./bin
	@mkdir -p $(GOBIN)
	@for cmd in $(CMDS); do \
		echo "  build $$cmd"; \
		GOOS= GOARCH=$(GOARCH) go build -o $(GOBIN)/$$cmd ./core/cmd/$$cmd || exit 1; \
	done

.PHONY: test
test: ## Run the unit tests
	GOARCH=$(GOARCH) go test $(PKGS)

.PHONY: test-race
test-race: ## Run the unit tests under the race detector
	GOARCH=$(GOARCH) go test -race $(PKGS)

.PHONY: cover
cover: ## Report coverage per package
	GOARCH=$(GOARCH) go test -coverprofile=coverage.out $(PKGS)
	GOARCH=$(GOARCH) go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	GOARCH=$(GOARCH) go vet $(PKGS)

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
check: fmt-check vet test ## Everything CI runs

.PHONY: clean
clean:
	rm -rf $(GOBIN) coverage.out

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
