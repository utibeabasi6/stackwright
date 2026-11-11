PNPM := npx --yes pnpm@10.34.7
TOOL := go tool -modfile=tools/go.mod
COMPOSE := docker compose -f compose.yaml -f compose.dev.yaml
export GOTOOLCHAIN := local

-include .env
export STACKWRIGHT_POSTGRES_PASSWORD STACKWRIGHT_APP_DB_PASSWORD STACKWRIGHT_NATS_PASSWORD STACKWRIGHT_S3_ACCESS_KEY STACKWRIGHT_S3_SECRET_KEY STACKWRIGHT_KEK

.PHONY: init up down dev test test-go test-web lint lint-go lint-web lint-proto generate build images secrets vuln licenses smoke

init:
	docker build -q --target binary -t stackwright/stackwright:init . >/dev/null
	docker run --rm --user $$(id -u):$$(id -g) -v "$(CURDIR):/work" -w /work stackwright/stackwright:init init

up:
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down

dev: up
	cd web && $(PNPM) dev

test: test-go test-web

test-go:
	STACKWRIGHT_TEST_INFRA=1 STACKWRIGHT_TEST_ENGINES=1 go test -race -count=1 ./...

test-web:
	cd web && $(PNPM) install --frozen-lockfile && $(PNPM) test

lint: lint-go lint-web lint-proto

lint-go:
	golangci-lint run ./...

lint-web:
	cd web && $(PNPM) install --frozen-lockfile && $(PNPM) lint && $(PNPM) typecheck

lint-proto:
	$(TOOL) buf lint
	$(TOOL) buf format --diff --exit-code

generate:
	cd web && $(PNPM) install --frozen-lockfile
	$(TOOL) buf generate
	cd web && $(PNPM) tokens

build:
	go build ./...
	cd web && $(PNPM) build

images:
	docker build -t stackwright/stackwright:dev .
	docker build --target executor -t stackwright/executor:dev .

secrets:
	$(TOOL) gitleaks git --no-banner --redact .

vuln:
	$(TOOL) govulncheck ./...

licenses:
	$(TOOL) go-licenses check ./... --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0
	cd web && $(PNPM) licenses list --prod --json | node scripts/licenses.mjs

smoke:
	$(COMPOSE) up -d --build --wait
	curl -fsS http://127.0.0.1:8080/healthz
	curl -fsS -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/stackwright.v1.MetaService/GetVersion
	curl -fsS http://127.0.0.1:8080/ | grep -q '<div id="root">'
