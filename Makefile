.PHONY: dashboard build build-race check test test-go test-go-race test-contracts test-integration test-browser test-race-e2e test-sandbox package audit

# PYTHONUNBUFFERED streams Python's otherwise pipe-buffered PASS lines under make and CI.
# E2E scenarios run with up to four workers; OCTOMUS_TEST_JOBS overrides the limit.
E2E_ENV = OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" PYTHONUNBUFFERED=1

# -shuffle=on randomizes test and package order to catch order-dependent state;
# a failure prints its seed (-test.shuffle N) to reproduce.
GO_TEST = go test -timeout 30m -shuffle=on ./...
GO_TEST_RACE = CGO_ENABLED=1 go test -race -timeout 30m -shuffle=on ./...

define GO_TESTS
	$(GO_TEST)
	$(GO_TEST_RACE)
endef

define CONTRACT_CORE
	$(E2E_ENV) python3 tests/binary_contract.py
	$(E2E_ENV) python3 tests/evidence_snapshot.py
	node --test tests/helpers/public_payload.test.mjs
endef

define CONTRACT_PACKAGE
	$(E2E_ENV) python3 tests/distribution.py
	python3 tests/package_guards.py
endef

dashboard:
	npm run build --prefix web

build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/octomus-agent ./cmd/octomus-agent

# Race-instrumented build used by test-race-e2e; the release binary stays CGO_ENABLED=0.
build-race: dashboard
	CGO_ENABLED=1 go build -race -o bin/octomus-agent-race ./cmd/octomus-agent

check: dashboard
	files=$$("$$(go env GOROOT)/bin/gofmt" -l version.go cmd internal tests web/*.go) || exit 1; \
	if [ -n "$$files" ]; then printf 'gofmt required:\n%s\n' "$$files" >&2; exit 1; fi
	go vet ./...
	npm run check --prefix web
	npm run format:check --prefix web
	cd web && node_modules/.bin/prettier --config .prettierrc.json --check ../tests/helpers
	cd web && node_modules/.bin/tsc --noEmit --allowJs --checkJs --strict --target es2022 \
	  --module nodenext --moduleResolution nodenext --types node ../tests/helpers/*.mjs

test: build
	$(GO_TESTS)
	$(CONTRACT_CORE)
	$(E2E_ENV) python3 tests/integration.py $(INTEGRATION_SCENARIOS)
	$(CONTRACT_PACKAGE)
	$(E2E_ENV) npm test --prefix web -- $(PLAYWRIGHT_ARGS)

test-go: dashboard
	$(GO_TEST)

test-go-race: dashboard
	$(GO_TEST_RACE)

test-contracts: build
	$(CONTRACT_CORE)
	$(CONTRACT_PACKAGE)

test-integration: build
	$(E2E_ENV) python3 tests/integration.py $(INTEGRATION_SCENARIOS)

test-browser: build
	$(E2E_ENV) npm test --prefix web -- $(PLAYWRIGHT_ARGS)

# Opt-in (~7 min): kept out of `make test` because the race runtime perturbs the other suites' timing.
test-race-e2e: build-race
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent-race" GORACE=halt_on_error=1 PYTHONUNBUFFERED=1 python3 tests/e2e.py

# Opt-in: needs a Docker Engine 28+ daemon. Runs the broker against the real daemon, then the shipped compose stack
# end to end with fixture runners inside real sandboxes.
test-sandbox: dashboard
	OCTOMUS_DOCKER_TEST=1 go test -count=1 ./internal/sandbox/...
	PYTHONUNBUFFERED=1 python3 tests/e2e_sandbox.py

# Pin govulncheck's toolchain to this module's: `go run pkg@version` would otherwise select govulncheck's own (possibly older) go.mod toolchain.
audit:
	toolchain=$$(go env GOVERSION | cut -d' ' -f1); \
	case $$toolchain in go1.*) export GOTOOLCHAIN=$$toolchain ;; esac; \
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	npm audit --prefix web --audit-level=high

package: build
	@arch=$$(go env GOARCH); \
	case $$arch in \
	  amd64) target=x86_64-unknown-linux-gnu ;; \
	  arm64) target=aarch64-unknown-linux-gnu ;; \
	  *) echo "Unsupported release architecture: $$arch" >&2; exit 1 ;; \
	esac; \
	./scripts/package.sh "v$$(cat VERSION)" "$$target" bin/octomus-agent
	cd dist && sha256sum octomus-agent-*.tar.gz > SHA256SUMS
