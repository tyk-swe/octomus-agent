.PHONY: dashboard build check test test-go test-go-race test-ui-logic test-python test-integration test-browser test-sandbox production-image-driver test-production-images package audit

# PYTHONUNBUFFERED streams Python's otherwise pipe-buffered PASS lines under make and CI.
# Scenarios run with up to four workers; OCTOMUS_TEST_JOBS overrides the limit.
E2E_ENV = OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" PYTHONUNBUFFERED=1

# -shuffle=on randomizes test and package order to catch order-dependent state;
# a failure prints its seed (-test.shuffle N) to reproduce.
GO_TEST_FLAGS = -timeout 30m -shuffle=on
GO_TEST_PACKAGES ?= ./...

# Directory timestamps also invalidate the build when inputs are added or removed.
# Vite reads VERSION, and Makefile changes can alter the input list or build command.
DASHBOARD_INPUTS = $(shell find web/src web/static -type f -o -type d) VERSION Makefile \
	$(addprefix web/,package.json package-lock.json svelte.config.js vite.config.ts tsconfig.json)

dashboard: web/build/200.html

web/build/200.html: $(DASHBOARD_INPUTS)
	npm run build --prefix web

build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/octomus-agent ./cmd/octomus-agent

check: dashboard
	files=$$("$$(go env GOROOT)/bin/gofmt" -l version.go cmd internal web/*.go tests/productionimage) || exit 1; \
	if [ -n "$$files" ]; then printf 'gofmt required:\n%s\n' "$$files" >&2; exit 1; fi
	go vet ./...
	npm run check --prefix web
	npm run format:check --prefix web

# The race suite runs the same tests as test-go, with the race detector; test-go is the quick local loop.
test:
	$(MAKE) test-go-race
	$(MAKE) test-integration
	$(MAKE) test-browser

test-go: dashboard
	go test $(GO_TEST_FLAGS) $(GO_TEST_PACKAGES)

# -race also turns on checkptr, which spends most of its time in the pure-Go SQLite driver's
# unsafe code; this module has none of its own.
test-go-race: dashboard
	CGO_ENABLED=1 go test -race -gcflags='modernc.org/...=-d=checkptr=0' $(GO_TEST_FLAGS) $(GO_TEST_PACKAGES)

# Pure dashboard rules: needs npm dependencies, but no dashboard build, service or browser.
test-ui-logic:
	npm run test:unit --prefix web -- $(PLAYWRIGHT_ARGS)

test-python:
	$(E2E_ENV) python3 -m unittest discover -s tests -p 'test_*.py'

test-integration: build test-python
	$(E2E_ENV) python3 tests/distribution.py
	$(E2E_ENV) python3 tests/e2e.py $(SCENARIOS)

test-browser: build
	$(E2E_ENV) npm test --prefix web -- $(PLAYWRIGHT_ARGS)

# Opt-in: needs a Docker Engine 28+ daemon. Runs the broker against the real daemon, then the shipped compose stack
# end to end with fixture runners inside real sandboxes.
test-sandbox: dashboard
	OCTOMUS_DOCKER_TEST=1 go test -count=1 ./internal/sandbox/...
	PYTHONUNBUFFERED=1 python3 tests/e2e_sandbox.py

# Real pinned clients in the exact retained production OCI images. Native Docker Engine 28+, Compose, Skopeo and
# OpenSSL are required; use a disposable host because the fixture reserves a globally numbered local Docker subnet.
# CI provides dist/images from the production Dockerfiles and requires both native release architectures.
production-image-driver:
	CGO_ENABLED=0 go test -c -o bin/production-image-contract ./tests/productionimage

test-production-images: production-image-driver
	PYTHONUNBUFFERED=1 python3 tests/production_images.py

# Pin govulncheck's toolchain to this module's: `go run pkg@version` would otherwise select govulncheck's own (possibly older) go.mod toolchain.
audit:
	toolchain=$$(go env GOVERSION | cut -d' ' -f1); \
	case $$toolchain in go1.*) export GOTOOLCHAIN=$$toolchain ;; esac; \
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	npm audit --prefix web --audit-level=high

package: build
	./scripts/package.sh
