.PHONY: dashboard build build-race check test test-race-e2e package audit

# Integration suites run the freshly built binary and stream their PASS lines
# (Python block-buffers stdout when it is a pipe, as under make and CI).
E2E_ENV = OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" PYTHONUNBUFFERED=1

dashboard:
	npm run build --prefix web

build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/octomus-agent ./cmd/octomus-agent

# Race-instrumented service build used by `make test-race-e2e`; the release
# binary stays CGO_ENABLED=0.
build-race: dashboard
	CGO_ENABLED=1 go build -race -o bin/octomus-agent-race ./cmd/octomus-agent

# gofmt comes from the module's toolchain, not PATH; parse errors fail too.
check: dashboard
	files=$$("$$(go env GOROOT)/bin/gofmt" -l version.go cmd internal tests web/*.go) || exit 1; \
	if [ -n "$$files" ]; then printf 'gofmt required:\n%s\n' "$$files" >&2; exit 1; fi
	go vet ./...
	npm run check --prefix web
	npm run format:check --prefix web

test: build
	go test ./...
	CGO_ENABLED=1 go test -race ./...
	$(E2E_ENV) python3 tests/binary_contract.py
	$(E2E_ENV) python3 tests/evidence_snapshot.py
	$(E2E_ENV) python3 tests/e2e.py
	$(E2E_ENV) python3 tests/e2e_baseline.py
	$(E2E_ENV) python3 tests/e2e_notifications.py
	$(E2E_ENV) python3 tests/e2e_runners.py
	$(E2E_ENV) python3 tests/e2e_hardening.py
	$(E2E_ENV) python3 tests/distribution.py
	python3 tests/package_guards.py
	$(E2E_ENV) npm test --prefix web

# Opt-in (about 7 minutes; needs a C compiler): the core integration suite
# against the race-instrumented service, so real HTTP, scheduler and runner
# subprocess interleavings reach the race detector. A detected race exits the
# service with status 66, which fails the scenario (the harness checks it on
# every stop, shutdown races included). Not part of `make test`: the race
# runtime can perturb the timing assertions of the other suites.
test-race-e2e: build-race
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent-race" GORACE=halt_on_error=1 PYTHONUNBUFFERED=1 python3 tests/e2e.py

# `go run pkg@version` selects a toolchain from govulncheck's own go.mod, which
# can be older than this module's and then cannot type-check it; pin the
# toolchain this module resolves to (GOTOOLCHAIN auto-switching included).
audit:
	toolchain=$$(go env GOVERSION | cut -d' ' -f1); \
	case $$toolchain in go1.*) export GOTOOLCHAIN=$$toolchain ;; esac; \
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	npm audit --prefix web --audit-level=high

# The version comes from the VERSION file (v<VERSION>-<target> archive names);
# Go architectures map onto the archive target labels the installer understands.
package: build
	@arch=$$(go env GOARCH); \
	case $$arch in \
	  amd64) target=x86_64-unknown-linux-gnu ;; \
	  arm64) target=aarch64-unknown-linux-gnu ;; \
	  *) echo "Unsupported release architecture: $$arch" >&2; exit 1 ;; \
	esac; \
	./scripts/package.sh "v$$(cat VERSION)" "$$target" bin/octomus-agent
	cd dist && sha256sum octomus-agent-*.tar.gz > SHA256SUMS
