.PHONY: build check test package

build:
	cargo build --release --locked
	npm ci --prefix web
	npm run build --prefix web

check:
	cargo fmt --check
	cargo clippy --all-targets --locked -- -D warnings
	npm run check --prefix web
	npm run format:check --prefix web

test:
	cargo test --locked
	cargo build --locked
	npm run build --prefix web
	python3 tests/e2e.py
	npm test --prefix web

package: build
	rm -rf dist/octomus-agent
	mkdir -p dist/octomus-agent/web
	cp target/release/octomus-agent dist/octomus-agent/
	cp -R web/build dist/octomus-agent/web/
	cp -R deploy docs dist/octomus-agent/
	cp README.md todo.md LICENSE PRD.md dist/octomus-agent/
	tar -czf dist/octomus-agent-linux-$$(uname -m).tar.gz -C dist octomus-agent
