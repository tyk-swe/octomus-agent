.PHONY: dashboard build check test package audit

dashboard:
	npm run build --prefix web

build: dashboard
	cargo build --release --locked

check: dashboard
	cargo fmt --check
	cargo clippy --all-targets --locked -- -D warnings
	npm run check --prefix web
	npm run format:check --prefix web

test: dashboard
	cargo test --locked
	cargo build --locked
	python3 tests/e2e.py
	python3 tests/e2e_runners.py
	python3 tests/distribution.py
	python3 tests/crate_guards.py
	npm test --prefix web

audit:
	cargo audit
	npm audit --prefix web --audit-level=high

package: build
	./scripts/package.sh v$$(sed -n 's/^version = "\(.*\)"/\1/p' Cargo.toml | head -1) $$(rustc -vV | sed -n 's/^host: //p') target/release/octomus-agent
	cd dist && sha256sum octomus-agent-*.tar.gz > SHA256SUMS
