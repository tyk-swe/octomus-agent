.PHONY: dashboard build check test package audit

dashboard:
	npm run build --prefix web

build: dashboard
	cargo build --release --locked

check: dashboard
	cargo fmt --check
	cargo clippy --all-targets --locked -- -D warnings
	npm run check --prefix web
	npm run showcase:check --prefix web
	npm run site:check --prefix web
	npm run format:check --prefix web

test: dashboard
	cargo test --locked
	cargo build --locked
	python3 tests/compatibility_capture.py
	python3 tests/evidence_snapshot.py
	python3 tests/e2e.py
	python3 tests/e2e_baseline.py
	python3 tests/e2e_notifications.py
	python3 tests/e2e_runners.py
	python3 tests/e2e_hardening.py
	python3 tests/distribution.py
	python3 tests/crate_guards.py
	npm test --prefix web
	npm run showcase:test --prefix web
	npm run site:test --prefix web

audit:
	cargo audit
	npm audit --prefix web --audit-level=high

package: build
	./scripts/package.sh v$$(sed -n 's/^version = "\(.*\)"/\1/p' Cargo.toml | head -1) $$(rustc -vV | sed -n 's/^host: //p') target/release/octomus-agent
	cd dist && sha256sum octomus-agent-*.tar.gz > SHA256SUMS

# Migration targets only; Rust remains the default through M8.
.PHONY: build-go check-go test-go test-go-storage
build-go: dashboard
	CGO_ENABLED=0 go build -trimpath -o bin/octomus-agent-go ./cmd/octomus-agent

check-go: dashboard
	test -z "$$(gofmt -l cmd internal tests/go web/embed*.go)"
	go vet ./...

test-go: build-go
	go test ./...
	CGO_ENABLED=1 go test -race ./...
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent-go" python3 tests/go_foundations.py --go-m1
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent-go" python3 tests/evidence_snapshot.py

# Cross-language storage checks need the frozen Rust reference debug binary
# (cargo build --locked) next to the Go executable; they run strictly serially.
test-go-storage: build-go
	cargo build --locked
	OCTOMUS_TEST_BINARY="$(CURDIR)/bin/octomus-agent-go" python3 tests/go_storage.py
