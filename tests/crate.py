#!/usr/bin/env python3
"""After cargo package, build its extracted sources and smoke-test that exact binary."""
import json
from pathlib import Path
import subprocess
import tomllib
from distribution import smoke

project = Path(__file__).resolve().parents[1]
with (project / 'Cargo.toml').open('rb') as file:
    version = tomllib.load(file)['package']['version']
package = project / 'target/package' / f'octomus-agent-{version}'
assert (package / 'web/build/200.html').is_file(), 'Crate omitted the dashboard'
assert not (package / 'web/package.json').exists(), 'Crate should not need an npm build'
assert not (package / 'web/node_modules').exists(), 'Crate must not bundle Node dependencies'
result = subprocess.run(['cargo', 'build', '--locked', '--manifest-path', str(package / 'Cargo.toml'),
                         '--target-dir', str(project / 'target'), '--message-format=json'],
                        cwd=package, check=True, capture_output=True, text=True)
artifacts = [json.loads(line) for line in result.stdout.splitlines()]
artifact = next(a for a in artifacts if a.get('executable') and a['target']['name'] == 'octomus-agent')
assert Path(artifact['target']['src_path']).resolve() == package / 'src/main.rs'
smoke(Path(artifact['executable']))
print('PASS extracted crate: builds from packaged sources/assets and serves without Node on PATH')
