# puckview — build & dev helpers.
#
# The Ansible role DEPLOYS the artifact built here; it does not build. Before
# deploying, run `make arm64` (cross-compiles for the puck and refreshes the OUI
# table) — the role copies dist/puckview-linux-arm64 to the box.
BINARY  := puckview
VERSION ?= 1.0.2
GOARCH  ?= arm64
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist/$(BINARY)-linux-$(GOARCH)

.PHONY: run build arm64 oui test vet fmt tidy clean

run: ## run locally against a throwaway DB (loopback :8091)
	PUCKVIEW_DB=/tmp/puckview-dev.db go run ./cmd/puckview

build: ## build for the host platform
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/puckview

arm64: oui ## cross-compile the static deploy binary -> dist/ (the puck target)
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST) ./cmd/puckview
	@echo "built $(DIST)"

oui: ## refresh the embedded IEEE OUI vendor table (cached, offline-safe)
	./scripts/update-oui.sh

test: ## run unit tests
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt the tree
	gofmt -w .

tidy: ## tidy modules
	go mod tidy

clean:
	rm -rf $(BINARY) dist
