.PHONY: swag lint lint-config build build-synth test test-coverage debug-synth-canary debug-synth-now help

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
SWAG_VERSION := v1.16.4
GOLANGCI_BASE_URL := https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/main/go/.golangci.base.yml

swag:
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)
	@# Portable BSD/GNU sed: `-i.bak` + rm so macOS + Alpine both work.
	@sed -i.bak 's|^//	@version.*|//	@version		$(VERSION)|' cmd/server/main.go
	@rm -f cmd/server/main.go.bak
	swag init -g cmd/server/main.go -o docs

lint-config:
	@command -v yq >/dev/null || { echo "yq required (brew install yq)"; exit 1; }
	@curl -fsSL -o .golangci.base.yml $(GOLANGCI_BASE_URL)
	@yq eval-all '. as $$item ireduce ({}; . *+ $$item)' .golangci.base.yml .golangci.yml > .golangci.merged.yml

lint: lint-config
	golangci-lint run --config .golangci.merged.yml ./...

build: swag build-synth
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/server ./cmd/server

# Build the synth CLI (the actual workload — invoked by CronJob).
build-synth:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/synth ./cmd/synth

test:
	go test ./... -v -count=1 -race

test-coverage:
	go test ./... -v -count=1 -race -coverprofile=cover.out
	@TOTAL=$$(go tool cover -func=cover.out | awk '/^total:/ {print $$3}' | sed 's/%//'); \
	THRESHOLD=60.0; \
	echo "coverage: $$TOTAL% (threshold: $$THRESHOLD%)"; \
	awk -v t="$$TOTAL" -v th="$$THRESHOLD" 'BEGIN { exit !(t < th) }' && echo "FAIL: below threshold" && exit 1 || true; \
	echo "PASS"

# ── Debug Pod recipes ────────────────────────────────────────────────────
# Run the synth binary in-cluster against real Tempo + GCS. Prefer these
# over local port-forwarding — same code path as production, multi-cluster
# trivial via CLUSTER var, no leftover state (`--rm -i`).
#
# Pattern documented in ~/leartech/qa-architecture/session-0-lessons.md
# under "Pattern: debug pods over local port-forwarding".
#
# Usage:
#   make debug-synth-canary CLUSTER=gke_product-first_us-east1-b_tf-jx-usable-bird TAG=0.0.1
#   make debug-synth-now SERVICE=leartech-broker-ui CLUSTER=modern-burro TAG=0.0.1

CLUSTER ?= gke_product-first_us-east1-b_tf-jx-usable-bird
TAG ?= latest
IMAGE := us-central1-docker.pkg.dev/product-first/oci/tempo-to-har:$(TAG)

debug-synth-canary:                        ## Synth HAR for canary (last 60min, dry-run)
	kubectl --context=$(CLUSTER) -n jx-staging run tempo-to-har-debug-$$$$ \
		--image=$(IMAGE) \
		--rm -i --restart=Never \
		--env="SERVICE_NAME=leartech-qa-canary" \
		--env="WINDOW_MINUTES=60" \
		--env="CLUSTER_TAG=$(CLUSTER)" \
		--command -- /synth --dry-run

debug-synth-now:                           ## Synth HAR for $(SERVICE) — real upload to GCS
	@test -n "$(SERVICE)" || { echo "SERVICE required"; exit 1; }
	kubectl --context=$(CLUSTER) -n jx-staging run tempo-to-har-debug-$$$$ \
		--image=$(IMAGE) \
		--rm -i --restart=Never \
		--env="SERVICE_NAME=$(SERVICE)" \
		--env="CLUSTER_TAG=$(CLUSTER)" \
		--command -- /synth

help:                                      ## Show this message
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-25s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
