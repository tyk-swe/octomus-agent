.PHONY: dashboard build check test test-go test-go-race test-integration test-browser test-sandbox package audit

# PYTHONUNBUFFERED streams Python's otherwise pipe-buffered PASS lines under make and CI.
# Scenarios run with up to four workers; OCTOMUS_TEST_JOBS overrides the limit.
E2E_ENV = OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" PYTHONUNBUFFERED=1

# -shuffle=on randomizes test and package order to catch order-dependent state;
# a failure prints its seed (-test.shuffle N) to reproduce.
GO_TEST_FLAGS = -timeout 30m -shuffle=on

dashboard:
	npm run build --prefix web

build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/octomus-agent ./cmd/octomus-agent

check: dashboard
	files=$$("$$(go env GOROOT)/bin/gofmt" -l version.go cmd internal web/*.go) || exit 1; \
	if [ -n "$$files" ]; then printf 'gofmt required:\n%s\n' "$$files" >&2; exit 1; fi
	go vet ./...
	npm run check --prefix web
	npm run format:check --prefix web

test: test-go test-go-race test-integration test-browser

test-go: dashboard
	go test $(GO_TEST_FLAGS) ./...

test-go-race: dashboard
	CGO_ENABLED=1 go test -race $(GO_TEST_FLAGS) ./...

test-integration: build
	$(E2E_ENV) python3 tests/distribution.py
	$(E2E_ENV) python3 tests/e2e.py $(SCENARIOS)

test-browser: build
	$(E2E_ENV) npm test --prefix web -- $(PLAYWRIGHT_ARGS)

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
	./scripts/package.sh
