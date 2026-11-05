.DEFAULT_GOAL := build

OBOT_REF ?= main
OBOT_INSTANCE ?= dev
OBOT_INSTANCES ?= 1
UPGRADE_FROM ?=
UPGRADE_TO ?= main
TESTARGS ?=
TFARGS ?=
DOCS_PORT ?= 8765
DOCS_PREVIEW_ENV = .obot/docs-preview/venv
ACCEPTANCE_TEST = go test -count=1 -timeout=60m -run=TestAcc $(TESTARGS) ./internal/provider/...

help:
	@printf '%s\n' \
		'make test                       Run Go and Python unit tests; no Obot needed' \
		'make test-acceptance            Run against existing Obot instance dev' \
		'  OBOT_INSTANCE=NAME            Select another existing instance' \
		'make test-acceptance-fresh      Create test instances, test, then remove them' \
		'  OBOT_REF=main|v0.26.3         Select the server and CLI version' \
		'  OBOT_INSTANCES=2              Include tests across multiple instances' \
		'make test-smoke                 Create instances, check auth, then remove them' \
		'make test-e2e                   Drive the compiled provider through tests/e2e' \
		'  OBOT_REF=main|v0.27.0         Select the server and CLI version' \
		'make test-upgrade               Apply, upgrade Obot in place, expect no changes' \
		'  UPGRADE_FROM=v0.27.0 UPGRADE_TO=main' \
		'make obot-versions              Show the Obot versions CI tests (obot-versions.json)' \
		'make test-acceptance-env        Use OBOT_BASE_URL, OBOT_TOKEN, OBOT_TEST_EMAIL' \
		'  TESTARGS="-run=TestAccNAME"    Select acceptance tests' \
		'make obot-up / obot-down        Start / remove named instance (default: dev)' \
		'make NAME-plan                 Preview Terraform changes against NAME' \
		'make NAME-apply                Apply changes with a confirmation prompt' \
		'make NAME-plan-destroy         Preview removal of Terraform-managed objects' \
		'make NAME-destroy              Remove managed objects with confirmation' \
		'  Replace NAME with instance name, for example: make dev-plan' \
		'make docs-preview               Preview provider docs with the standard light theme' \
		'  DOCS_PORT=8765                Override the preview port' \
		'make build / lint / generate    Build, lint, or regenerate provider docs'

build:
	mkdir -p bin
	go build -o bin/terraform-provider-obot .

install:
	go install .

lint:
	golangci-lint run

generate:
	cd tools && go generate ./...

# Pin preview dependencies in an isolated, ignored environment.
.obot/docs-preview/ready: scripts/docs-preview-requirements.txt
	python3 -m venv "$(DOCS_PREVIEW_ENV)"
	"$(DOCS_PREVIEW_ENV)/bin/python" -m pip install -r scripts/docs-preview-requirements.txt
	touch $@

docs-preview: .obot/docs-preview/ready
	"$(DOCS_PREVIEW_ENV)/bin/python" scripts/docs_preview.py --port "$(DOCS_PORT)"

# Format only the project source directories.
fmt:
	gofmt -s -w main.go internal tools

test: test-go test-tooling

test-go:
	TF_ACC= go test -cover -timeout=120s ./...

test-acceptance:
	python3 scripts/obot.py exec "$(OBOT_INSTANCE)" -- env TF_ACC=1 $(ACCEPTANCE_TEST)

test-acceptance-env:
	TF_ACC=1 $(ACCEPTANCE_TEST)

test-acceptance-fresh:
	python3 scripts/obot.py testacc --ref "$(OBOT_REF)" --instances "$(OBOT_INSTANCES)" -- $(ACCEPTANCE_TEST)

test-smoke:
	python3 scripts/obot.py smoke --ref "$(OBOT_REF)" --instances "$(OBOT_INSTANCES)"

test-e2e:
	python3 scripts/e2e.py e2e --ref "$(OBOT_REF)"

test-upgrade:
	@test -n "$(UPGRADE_FROM)" || { echo 'Set UPGRADE_FROM, for example UPGRADE_FROM=v0.27.0'; exit 2; }
	python3 scripts/e2e.py upgrade --from "$(UPGRADE_FROM)" --to "$(UPGRADE_TO)"

obot-versions:
	python3 scripts/obot_versions.py show

test-tooling:
	python3 -m unittest discover -s scripts -p 'test_*.py'

obot-source:
	python3 scripts/obot.py source --ref "$(OBOT_REF)"

obot-cli:
	python3 scripts/obot.py cli --ref "$(OBOT_REF)"

obot-up:
	python3 scripts/obot.py up "$(OBOT_INSTANCE)" --ref "$(OBOT_REF)"

obot-down:
	python3 scripts/obot.py down "$(OBOT_INSTANCE)"

# Compatibility aliases for the original target names.
testacc: test-acceptance-env
testacc-local: test-acceptance-fresh
obot-smoke: test-smoke

# Keep development overrides local to this checkout.
dev: build
	@printf '%s\n' \
		'provider_installation {' \
		'  dev_overrides {' \
		'    "registry.terraform.io/obot-platform/obot" = "$(CURDIR)/bin"' \
		'  }' \
		'  direct {}' \
		'}' > dev.tfrc
	@echo 'Run: export TF_CLI_CONFIG_FILE="$(CURDIR)/dev.tfrc"'

dev-check: dev
	TF_CLI_CONFIG_FILE="$(CURDIR)/dev.tfrc" terraform -chdir=examples/dev providers schema -json > /dev/null
	TF_CLI_CONFIG_FILE="$(CURDIR)/dev.tfrc" terraform -chdir=examples/dev validate

# The target prefix is the existing Obot instance name. Each uses separate
# Terraform state while sharing the development example configuration.
%-plan: dev
	python3 scripts/obot.py terraform "$*" plan -- $(TFARGS)

%-apply: dev
	python3 scripts/obot.py terraform "$*" apply -- $(TFARGS)

%-plan-destroy: dev
	python3 scripts/obot.py terraform "$*" plan-destroy -- $(TFARGS)

%-destroy: dev
	python3 scripts/obot.py terraform "$*" destroy -- $(TFARGS)

.PHONY: help build install lint generate docs-preview fmt test test-go test-tooling \
	test-acceptance test-acceptance-env test-acceptance-fresh test-smoke test-e2e test-upgrade obot-versions \
	testacc testacc-local obot-smoke dev dev-check obot-source obot-cli obot-up obot-down
