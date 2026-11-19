# dumbwaiter — local development entrypoint.
#
# CI is meant to call these same targets, so `make ci` locally and a green
# pipeline mean the same thing.
#
# COMPATIBILITY: this Makefile must keep working on GNU Make 3.81 — that is the
# system `make` on macOS and Apple will not ship a newer one (GPLv3). Do not use
# .ONESHELL / .SHELLFLAGS (3.82+), `!=` / $(file ...) (4.0+), $(intcmp) / $(let)
# (4.4+). One command per recipe line; Make aborts on the first non-zero exit.

SHELL := /usr/bin/env bash
MAKEFLAGS += --no-print-directory
.DEFAULT_GOAL := help

MAKE_MAJOR := $(firstword $(subst ., ,$(MAKE_VERSION)))
ifneq ($(MAKE_MAJOR),3)
ifndef CI
$(info note: GNU Make $(MAKE_VERSION) — this Makefile targets 3.81 (macOS system make), keep it compatible)
endif
endif

BINARY_NAME  ?= dumbwaiter
MAIN_PKG     ?= ./cmd/dumbwaiter
DIST_DIR     ?= $(CURDIR)/dist
LOCALBIN     ?= $(CURDIR)/bin
GO           ?= go
DOCKER       ?= docker
KUBECTL      ?= kubectl
HELM         ?= helm
COVERPROFILE ?= cover.out
GOTEST_FLAGS ?= -v

# One version string everywhere: binary, image, chart, release. Only the git
# tag carries the "v" (v1.2.0), as Go modules require; the rest is 1.2.0.
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS ?= -s -w -X main.version=$(VERSION)

IMAGE     ?= ghcr.io/jtprogru/dumbwaiter
IMAGE_TAG ?= $(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

OVERLAYS  ?= gateway ingress tls
OVERLAY   ?= gateway
NAMESPACE ?=
# Every committed config, checked with placeholder S3 keys.
CONFIGS   ?= config.example.yaml $(wildcard deploy/kubernetes/base/config.yaml deploy/kubernetes/overlays/*/config.yaml deploy/compose/config*.yaml deploy/systemd/config.yaml deploy/monitoring/config.yaml)

CHART        ?= deploy/helm/dumbwaiter
# The chart is linted with its defaults and with each of these.
CHART_VALUES ?= $(wildcard $(CHART)/ci/*-values.yaml)
COMPOSE_DIR  ?= deploy/compose
COMPOSE_FILES ?= compose.yaml compose.tls.yaml

GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0

GOLANGCI_LINT := $(LOCALBIN)/golangci-lint
GOVULNCHECK   := $(LOCALBIN)/govulncheck

GOLANGCI_LINT_STAMP := $(LOCALBIN)/.golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK_STAMP   := $(LOCALBIN)/.govulncheck-$(GOVULNCHECK_VERSION)

# --- meta -------------------------------------------------------------------

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\n"} \
		/^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)

# --- build ------------------------------------------------------------------

run: ## Run via go run: make run ARGS="-config my.yaml"
	$(GO) run $(MAIN_PKG) $(ARGS)

build: ## Build the binary into ./dist
	$(GO) mod download
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME) $(MAIN_PKG)

tidy: ## go mod tidy
	$(GO) mod tidy

fmt: ## gofmt -s -w .
	gofmt -s -w .

vet: ## go vet ./...
	$(GO) vet ./...

# Only tracked files: worktrees under .claude/ are not ours to judge.
fmt-check: ## Fail if a tracked .go file needs gofmt -s
	@files=$$(gofmt -s -l $$(git ls-files '*.go')); if [ -n "$$files" ]; then echo "needs gofmt -s (run make fmt):"; echo "$$files"; exit 1; fi

tidy-check: ## Fail if go.mod or go.sum need go mod tidy
	$(GO) mod tidy -diff

# --- test / lint ------------------------------------------------------------

test: ## Short tests with coverage
	$(GO) test --short -coverprofile=$(COVERPROFILE) $(GOTEST_FLAGS) ./...

test-race: ## Race tests with coverage (this is what CI runs)
	$(GO) test -race -coverprofile=$(COVERPROFILE) $(GOTEST_FLAGS) ./...

lint: $(GOLANGCI_LINT_STAMP) ## golangci-lint run
	$(GOLANGCI_LINT) run $(GOLANGCI_LINT_FLAGS)

lint-fix: $(GOLANGCI_LINT_STAMP) ## golangci-lint run --fix
	$(GOLANGCI_LINT) run --fix

govulncheck: $(GOVULNCHECK_STAMP) ## Scan for known vulnerabilities
	$(GOVULNCHECK) ./...

check-configs: build ## Validate every committed YAML config
	@for f in $(CONFIGS); do \
		printf '%s: ' "$$f"; \
		S3_ACCESS_KEY_ID=placeholder S3_SECRET_ACCESS_KEY=placeholder $(DIST_DIR)/$(BINARY_NAME) -check -config "$$f" || exit 1; \
	done

ci: lint test-race ## Quick pre-push check: lint + race tests

ci-full: fmt-check tidy-check lint test-race govulncheck check-configs manifests helm-lint check-network-policies compose-config obs-check ## Static checks + Go tests (monitoring E2E: obs-up obs-smoke)

check-loadtest: ## Test k6 completion checks against a local slow upstream (k6 required)
	REQUIRE_LOAD_TOOLS=1 $(GO) test -count=1 ./internal/loadtest

check-network-policies: ## Assert admin isolation and HTTP/3 access in rendered policies (helm + kubectl required)
	REQUIRE_DEPLOY_TOOLS=1 $(GO) test -count=1 ./internal/deploytest

# --- release ----------------------------------------------------------------

# Release artifacts take the version from the tag. This pins it where the
# repository is used directly: the chart, the kustomize base, docker compose.
# A leading "v" is dropped, so VERSION=v1.2.0 works too.
set-version: SET_VERSION = $(patsubst v%,%,$(VERSION))
set-version: ## Pin a version in the chart, kustomize and compose: make set-version VERSION=1.2.0
	@echo '$(SET_VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo 'usage: make set-version VERSION=1.2.0, got "$(VERSION)"'; exit 1; }
	perl -pi -e 's/^version: .*/version: $(SET_VERSION)/; s/^appVersion: .*/appVersion: "$(SET_VERSION)"/' $(CHART)/Chart.yaml
	perl -pi -e 's/^(\s*newTag:) .*/$$1 "$(SET_VERSION)"/' deploy/kubernetes/base/kustomization.yaml
	perl -pi -e 's/(DUMBWAITER_TAG:-)[^}]*/$${1}$(SET_VERSION)/' $(addprefix $(COMPOSE_DIR)/,$(COMPOSE_FILES))
	perl -pi -e 's/^DUMBWAITER_TAG=.*/DUMBWAITER_TAG=$(SET_VERSION)/' $(COMPOSE_DIR)/.env.example
	@git --no-pager diff --stat -- $(CHART)/Chart.yaml deploy/kubernetes/base/kustomization.yaml $(COMPOSE_DIR)

# --- image ------------------------------------------------------------------

docker-build: ## Build the image for the local platform
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) .

docker-push: ## Build a multi-arch image and push it: make docker-push IMAGE_TAG=0.1.0
	$(DOCKER) buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) --push .

# --- kubernetes -------------------------------------------------------------

manifests: $(addprefix manifests-,$(OVERLAYS)) ## Render every overlay into ./dist/manifests

manifests-%:
	@mkdir -p $(DIST_DIR)/manifests
	$(KUBECTL) kustomize deploy/kubernetes/overlays/$* > $(DIST_DIR)/manifests/$*.yaml

# Server-side apply for every overlay: client-side apply merges Service ports
# by number and drops one of the two 443 entries in the tls overlay.
diff: ## Show what deploy would change: make diff OVERLAY=tls NAMESPACE=web
	$(if $(NAMESPACE),,$(error NAMESPACE is required, e.g. make $@ NAMESPACE=web))
	$(KUBECTL) diff --server-side -n $(NAMESPACE) -k deploy/kubernetes/overlays/$(OVERLAY) || [ $$? -eq 1 ]

deploy: ## Apply an overlay: make deploy OVERLAY=tls NAMESPACE=web
	$(if $(NAMESPACE),,$(error NAMESPACE is required, e.g. make $@ NAMESPACE=web))
	$(KUBECTL) apply --server-side -n $(NAMESPACE) -k deploy/kubernetes/overlays/$(OVERLAY)

# Lints the chart, then feeds the config.yaml it renders to `dumbwaiter -check`,
# with a placeholder for every ${VAR} the config references.
helm-lint: build ## Lint the Helm chart and validate the config it renders
	@mkdir -p $(DIST_DIR)
	@set -o pipefail; for f in "" $(CHART_VALUES); do \
		printf '%s: ' "$${f:-$(CHART) defaults}"; \
		$(HELM) lint --strict --quiet $(CHART) $${f:+-f $$f} || exit 1; \
		$(HELM) template ci $(CHART) $${f:+-f $$f} --show-only templates/configmap.yaml \
			| sed -n '/^  config.yaml: |/,$$p' | sed -e 1d -e 's/^    //' > $(DIST_DIR)/helm-config.yaml || exit 1; \
		env $$(grep -o '$${[A-Za-z_][A-Za-z0-9_]*}' $(DIST_DIR)/helm-config.yaml | sort -u | sed 's/^$${\(.*\)}$$/\1=placeholder/') \
			$(DIST_DIR)/$(BINARY_NAME) -check -config $(DIST_DIR)/helm-config.yaml || exit 1; \
	done

# --- docker compose ---------------------------------------------------------

compose-config: ## Validate the docker compose setups against .env.example
	@for f in $(COMPOSE_FILES); do \
		printf '%s: ' "$(COMPOSE_DIR)/$$f"; \
		S3_ACCESS_KEY_ID=placeholder S3_SECRET_ACCESS_KEY=placeholder \
			$(DOCKER) compose --project-directory $(COMPOSE_DIR) --env-file $(COMPOSE_DIR)/.env.example \
			-f $(COMPOSE_DIR)/$$f config --quiet || exit 1; \
		echo ok; \
	done

# --- local monitoring (independent from production Compose) ------------------

OBS_COMPOSE = $(DOCKER) compose -f deploy/monitoring/compose.yaml
# Keep the unit-test tool aligned with vmalert in deploy/monitoring/compose.yaml.
OBS_VM_VERSION = v1.156.0

obs-up: ## Build app and start local monitoring (Grafana on localhost:3333)
	VERSION=$(VERSION) $(OBS_COMPOSE) up -d --build
	@echo 'Grafana: http://localhost:3333 (admin/admin unless overridden)'
	@echo 'Targets: http://localhost:8429/targets; alerts: http://localhost:8880'
	@echo 'Run make obs-smoke to verify metrics, logs, dashboards and synthetic probe.'

obs-down: ## Stop local monitoring and k6, retaining data volumes
	$(OBS_COMPOSE) --profile load --profile tools down

obs-check: ## Validate monitoring configs, generated dashboards and alert unit tests
	$(OBS_COMPOSE) config --quiet
	python3 scripts/monitoring-dashboards.py --check
	$(OBS_COMPOSE) run --rm --no-deps k6-run run --quiet /scripts/profiles.test.js
	@for profile in smoke steady spike background; do \
		$(OBS_COMPOSE) run --rm --no-deps -e LOAD_PROFILE=$$profile -e LOAD_TIME_SCALE=1 k6-run inspect --include-system-env-vars /scripts/load.js > /dev/null || exit 1; \
	done
	$(OBS_COMPOSE) run --rm --no-deps vmagent -promscrape.config=/etc/vmagent/scrape.yaml -promscrape.config.dryRun
	$(OBS_COMPOSE) run --rm --no-deps vmalert -rule=/etc/vmalert/rules.yaml -dryRun
	$(OBS_COMPOSE) run --rm --no-deps vector validate --no-environment /etc/vector/vector.yaml
	$(DOCKER) run --rm -v "$(CURDIR)/deploy/monitoring/alerts:/rules:ro" victoriametrics/vmalert-tool:$(OBS_VM_VERSION) unittest --files=/rules/tests.yaml

obs-smoke: ## End-to-end monitoring checks against the running local stack (Python 3)
	python3 scripts/monitoring-smoke.py

obs-dashboards: ## Regenerate committed Grafana dashboards
	python3 scripts/monitoring-dashboards.py

obs-load-up: obs-up ## Start opt-in 72-hour k6 source, waves between 10 and 200 RPS
	$(OBS_COMPOSE) up -d --no-deps k6-background
	@echo 'Load test dashboard: http://localhost:3333/d/dumbwaiter-load'

obs-load-down: ## Stop only the background k6 source
	$(OBS_COMPOSE) stop k6-background

obs-load-check: ## Short E2E checks of k6 profiles and remote write (requires obs-up)
	DOCKER="$(DOCKER)" python3 scripts/monitoring-load-check.py

obs-load-logs: ## Follow the background k6 run and its final result
	$(OBS_COMPOSE) logs --tail=50 -f k6-background

LOAD_PROFILE ?= smoke
LOAD_TIME_SCALE ?= 1

test-load: ## Run containerized k6: LOAD_PROFILE=smoke|steady|spike (requires obs-up)
	@case "$(LOAD_PROFILE)" in smoke|steady|spike) ;; *) echo 'LOAD_PROFILE must be smoke, steady or spike'; exit 1 ;; esac
	LOAD_PROFILE=$(LOAD_PROFILE) LOAD_TIME_SCALE=$(LOAD_TIME_SCALE) $(OBS_COMPOSE) run --rm --no-deps k6-run

test-load-smoke: ## 30 seconds at 10 RPS
	$(MAKE) test-load LOAD_PROFILE=smoke

test-load-steady: ## 5 minutes at 100 RPS
	$(MAKE) test-load LOAD_PROFILE=steady

test-load-spike: ## 3 minutes: 10 -> 200 RPS, plateau, recovery to 10 RPS
	$(MAKE) test-load LOAD_PROFILE=spike

# --- tools ------------------------------------------------------------------

tools: $(GOLANGCI_LINT_STAMP) $(GOVULNCHECK_STAMP) ## Install pinned tools into ./bin

$(GOLANGCI_LINT_STAMP):
	@mkdir -p $(LOCALBIN)
	@rm -f $(LOCALBIN)/.golangci-lint-*
	GOBIN=$(LOCALBIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@touch $@

$(GOVULNCHECK_STAMP):
	@mkdir -p $(LOCALBIN)
	@rm -f $(LOCALBIN)/.govulncheck-*
	GOBIN=$(LOCALBIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@touch $@

clean: ## Remove build artifacts
	rm -rf $(DIST_DIR) $(LOCALBIN) $(COVERPROFILE)

.PHONY: help run build tidy fmt vet fmt-check tidy-check test test-race lint lint-fix govulncheck \
        check-configs ci ci-full set-version docker-build docker-push manifests diff deploy \
        helm-lint check-network-policies check-loadtest compose-config tools clean \
        obs-up obs-down obs-check obs-smoke obs-dashboards \
        obs-load-up obs-load-down obs-load-logs obs-load-check test-load test-load-smoke test-load-steady test-load-spike
