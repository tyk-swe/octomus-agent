#!/usr/bin/env python3
"""Exercise Cargo's real ignored-input behavior in a temporary, credential-free crate."""
import os
from pathlib import Path
import subprocess
import tempfile

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts/package-crate.sh'
with tempfile.TemporaryDirectory(prefix='octomus-crate-guard-') as directory:
    root = Path(directory)
    (root / 'src').mkdir()
    (root / 'docs').mkdir()
    (root / '.gitignore').write_text('/web/build/\n/target/\n*.db\n')
    (root / 'Cargo.toml').write_text('''[package]
name = "octomus-packaging-fixture"
version = "0.0.0"
edition = "2024"
license = "Apache-2.0"
description = "Local test fixture only; never published"
repository = "https://github.com/fixture/project"
include = ["src/**", "docs/**", "web/build/**", "Cargo.toml", "Cargo.lock"]
''')
    source = root / 'src/main.rs'
    original = 'fn main() { assert_eq!(include_str!("../web/build/200.html"), "fixture"); }\n'
    source.write_text(original)
    env = {**os.environ, 'CARGO_TARGET_DIR': str(root / 'target')}
    for args in [['cargo', 'generate-lockfile', '--offline'], ['git', 'init', '-q'],
                 ['git', 'add', '.'], ['git', '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.com',
                                      'commit', '-qm', 'Clean fixture source']]:
        subprocess.run(args, cwd=root, env=env, check=True)
    (root / 'web/build').mkdir(parents=True)
    (root / 'web/build/200.html').write_text('fixture')
    # Cargo itself rejects the otherwise-clean checkout's explicitly included ignored assets.
    ordinary = subprocess.run(['cargo', 'package', '--locked', '--no-verify'], cwd=root, env=env, capture_output=True, text=True)
    assert ordinary.returncode and 'not yet committed' in ordinary.stderr
    source.write_text(original + '// source drift\n')
    rejected = subprocess.run([str(SCRIPT)], cwd=root, env=env, capture_output=True, text=True)
    assert rejected.returncode and 'Commit reviewed source' in rejected.stderr
    source.write_text(original)
    stray = root / 'docs/fixture.db'
    stray.write_text('fixture only')
    rejected = subprocess.run([str(SCRIPT)], cwd=root, env=env, capture_output=True, text=True)
    assert rejected.returncode and 'Unexpected untracked package input' in rejected.stderr, rejected.stderr
    stray.unlink()
    subprocess.run([str(SCRIPT)], cwd=root, env=env, check=True)
    assert (root / 'target/package/octomus-packaging-fixture-0.0.0.crate').is_file()
    print('PASS crate guard: generated assets package; source drift and unexpected ignored files are rejected')
