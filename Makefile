.PHONY: dashboard build build-race check test package audit test-go-storage

dashboard:
	npm run build --prefix web

build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/octomus-agent ./cmd/octomus-agent

# Race-instrumented build for the M8 qualification scenarios; the release
# binary stays CGO_ENABLED=0.
build-race: dashboard
	CGO_ENABLED=1 go build -race -o bin/octomus-agent-race ./cmd/octomus-agent

check: dashboard
	test -z "$$(gofmt -l version.go cmd internal tests/go web/embed*.go)"
	go vet ./...
	npm run check --prefix web
	npm run showcase:check --prefix web
	npm run site:check --prefix web
	npm run format:check --prefix web

test: build
	go test ./...
	CGO_ENABLED=1 go test -race ./...
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/compatibility_capture.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/go_foundations.py --go-m1
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/evidence_snapshot.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/e2e.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/e2e_baseline.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/e2e_notifications.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/e2e_runners.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/e2e_hardening.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/distribution.py
	python3 tests/package_guards.py
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" npm test --prefix web
	npm run showcase:test --prefix web
	npm run site:test --prefix web

audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
	npm audit --prefix web --audit-level=high

# The version comes from the VERSION file (v<VERSION>-<target> archive names);
# Go architectures map onto the legacy target labels the installer understands.
package: build
	@arch=$$(go env GOARCH); \
	case $$arch in \
	  amd64) target=x86_64-unknown-linux-gnu ;; \
	  arm64) target=aarch64-unknown-linux-gnu ;; \
	  *) echo "Unsupported release architecture: $$arch" >&2; exit 1 ;; \
	esac; \
	./scripts/package.sh "v$$(cat VERSION)" "$$target" bin/octomus-agent
	cd dist && sha256sum octomus-agent-*.tar.gz > SHA256SUMS

# Frozen Rust reference comparison: cross-language storage checks need the
# reference debug binary (cargo build --locked) next to the Go executable and
# run strictly serially. The Rust tree stays buildable until M10 retires it.
test-go-storage: build
	cargo build --locked
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent" python3 tests/go_storage.py
