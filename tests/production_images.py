#!/usr/bin/env python3
"""Accept retained production OCI images on this native architecture, using real clients and synthetic TLS responses.

Requires Docker Engine 28+, Compose, Skopeo, OpenSSL and bin/production-image-contract. No live account or token is used.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('release_images', ROOT / 'scripts/release-images.py')
images = importlib.util.module_from_spec(spec)
spec.loader.exec_module(images)
PROVIDER_HOST = 'provider.octomus.test'
# These addresses exist only on an ephemeral internal Docker bridge. They are deliberately outside the gateway's
# private-address denylist, so the production address checks stay enabled. No packet is routed to their public owner.
PROVIDER_SUBNET = '11.255.254.0/24'
PROVIDER_IP = '11.255.254.2'


def run(*args, capture=False, check=True, env=None, timeout=120):
    result = subprocess.run([str(arg) for arg in args], cwd=ROOT, env=env, text=True,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(f'{args[0]} failed ({result.returncode}): ' + (result.stderr or 'see command output'))
    return result.stdout.strip() if capture and check else result


def wait_for(description, predicate, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.2)
    raise RuntimeError('Timed out: ' + description)


def provider_files(directory):
    ca, key, cert = directory / 'provider-ca.pem', directory / 'provider.key', directory / 'provider.pem'
    run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2',
        '-keyout', directory / 'ca.key', '-out', ca, '-subj', '/CN=Octomus acceptance CA',
        '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign', capture=True)
    run('openssl', 'req', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', directory / 'provider.csr',
        '-subj', '/CN=' + PROVIDER_HOST, capture=True)
    extensions = directory / 'provider.ext'
    extensions.write_text('subjectAltName=DNS:' + PROVIDER_HOST + '\nbasicConstraints=critical,CA:FALSE\n'
                          'keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n')
    run('openssl', 'x509', '-req', '-in', directory / 'provider.csr', '-CA', ca, '-CAkey', directory / 'ca.key',
        '-CAcreateserial', '-out', cert, '-days', '2', '-extfile', extensions, capture=True)
    # Public test server credentials, confined to this temporary directory; never uploaded as CI artifacts.
    key.chmod(0o644)
    return ca, cert, key


def runner_configs(directory):
    (directory / 'config.toml').write_text(
        'model_provider = "contract"\n[model_providers.contract]\nname = "Contract"\n'
        f'base_url = "https://{PROVIDER_HOST}/codex/v1"\nwire_api = "responses"\n'
        'requires_openai_auth = false\nsupports_websockets = false\n')
    (directory / 'opencode.json').write_text(json.dumps({
        'enabled_providers': ['contract'], 'provider': {'contract': {
            'npm': '@ai-sdk/openai-compatible', 'name': 'Contract',
            'options': {'baseURL': f'https://{PROVIDER_HOST}/opencode/v1', 'apiKey': 'unused-contract-placeholder'},
            'models': {'contract-model': {'name': 'Contract model', 'limit': {'context': 8192, 'output': 1024},
                                          'tool_call': True, 'variants': {'high': {'temperature': 0.1}}}},
        }},
    }))


def overlay(project, image_refs, directory, volumes):
    """Only host-owned deployment values change; production broker policies and container specs remain in use."""
    return {
        'services': {
            'sandboxd': {
                'environment': {
                    'OCTOMUS_SANDBOX_DATA_VOLUME': volumes['data'],
                    'OCTOMUS_SANDBOX_RUNNER_VOLUME': volumes['runner'],
                    'OCTOMUS_SANDBOX_TOOLS_VOLUME': volumes['tools'],
                    'OCTOMUS_SANDBOX_RUNNER_NETWORK': project + '-runner',
                    'OCTOMUS_SANDBOX_VERIFY_NETWORK': project + '-verify',
                    'OCTOMUS_SANDBOX_INSTANCE': project,
                    'OCTOMUS_SANDBOX_CA_FILE': '/run/acceptance/provider-ca.pem',
                    'OCTOMUS_SANDBOX_MAX_SECONDS': '600',
                },
                'volumes': [{'type': 'bind', 'source': str(directory / 'provider-ca.pem'),
                             'target': '/run/acceptance/provider-ca.pem', 'read_only': True}],
            },
            'egress': {
                'extra_hosts': {PROVIDER_HOST: PROVIDER_IP},
                'networks': {'provider': {'ipv4_address': '11.255.254.3'}},
            },
            'provider': {
                'image': image_refs['octomus-sandbox'], 'pull_policy': 'never',
                'entrypoint': ['python3', '/provider.py', '/provider-status', '--host', '0.0.0.0', '--port', '443',
                               '--namespaced', '--tls-cert', '/provider.pem', '--tls-key', '/provider.key'],
                'user': '10001:10001', 'read_only': True, 'cap_drop': ['ALL'],
                'security_opt': ['no-new-privileges:true'], 'init': True,
                'tmpfs': ['/tmp:size=64m'], 'mem_limit': '128m', 'pids_limit': 64,
                'sysctls': {'net.ipv4.ip_unprivileged_port_start': '0'},
                'networks': {'provider': {'ipv4_address': PROVIDER_IP}},
                'volumes': [
                    {'type': 'bind', 'source': str(ROOT / 'tests/fixtures/provider.py'), 'target': '/provider.py', 'read_only': True},
                    {'type': 'bind', 'source': str(directory / 'provider.pem'), 'target': '/provider.pem', 'read_only': True},
                    {'type': 'bind', 'source': str(directory / 'provider.key'), 'target': '/provider.key', 'read_only': True},
                    {'type': 'bind', 'source': str(directory / 'status'), 'target': '/provider-status'},
                ],
            },
        },
        'volumes': {name: {'name': value, 'external': True} for name, value in volumes.items()},
        'networks': {
            'sandbox-runner': {'name': project + '-runner'},
            'sandbox-verify': {'name': project + '-verify'},
            'provider': {'name': project + '-provider', 'internal': True, 'enable_ipv6': False,
                         'driver_opts': {'com.docker.network.bridge.gateway_mode_ipv4': 'isolated'},
                         'ipam': {'config': [{'subnet': PROVIDER_SUBNET}]}},
        },
    }


def smoke_control_image(image_ref, name, version):
    """Boot the executable and embedded dashboard from the exact control-plane image with no live repository."""
    run('docker', 'run', '-d', '--name', name, '--read-only', '--cap-drop', 'ALL',
        '--security-opt', 'no-new-privileges:true', '--tmpfs', '/tmp:rw,nosuid,nodev,size=128m',
        '-e', 'OCTOMUS_TOKEN=production-image-contract-token-00000000', '-e', 'OCTOMUS_DATA_DIR=/tmp/state',
        '-p', '127.0.0.1::4200', image_ref, '--listen', '0.0.0.0:4200', '--sandbox', 'docker', capture=True)
    try:
        port = run('docker', 'port', name, '4200/tcp', capture=True).rsplit(':', 1)[1]
        base = 'http://127.0.0.1:' + port

        def healthy():
            try:
                with urllib.request.urlopen(base + '/healthz', timeout=2) as response:
                    return response.status == 200
            except (OSError, urllib.error.URLError):
                return False

        wait_for('production control-plane image health', healthy, 60)
        with urllib.request.urlopen(base + '/', timeout=5) as response:
            html = response.read().decode()
        asset = re.search(r'/_app/immutable/[^"\s)]+\.js', html)
        images.require(asset is not None, 'Production dashboard has no JavaScript entry point')
        with urllib.request.urlopen(base + asset.group(), timeout=5) as response:
            images.require(response.status == 200 and len(response.read()) > 100, 'Production dashboard asset missing')
        observed = run('docker', 'exec', name, '/usr/local/bin/octomus-agent', '--version', capture=True)
        images.require(observed == 'octomus-agent ' + version, 'Production control-plane version mismatch')
    finally:
        run('docker', 'logs', name, check=False)
        run('docker', 'rm', '-f', name, check=False, capture=True)


def inspect_topology(project, provider):
    network = json.loads(run('docker', 'network', 'inspect', project + '-provider', capture=True))[0]
    images.require(network.get('Internal') is True and network.get('EnableIPv6') is False and
                   network.get('Options', {}).get('com.docker.network.bridge.gateway_mode_ipv4') == 'isolated',
                   'The synthetic provider network is not isolated')
    container = json.loads(run('docker', 'inspect', provider, capture=True))[0]
    images.require(set(container['NetworkSettings']['Networks']) == {project + '-provider'} and
                   not container['HostConfig'].get('PortBindings') and
                   not any(container['NetworkSettings'].get('Ports', {}).values()),
                   'Synthetic provider must have only its internal network and no published ports')


def inspect_runner(project, volumes, image_id):
    identities = run('docker', 'ps', '-q', '--filter', 'label=octomus.sandbox.instance=' + project,
                     '--filter', 'label=octomus.sandbox.kind=runner', capture=True).splitlines()
    images.require(len(identities) == 1, 'Expected one active production runner during inspection')
    container = json.loads(run('docker', 'inspect', identities[0], capture=True))[0]
    images.require(container['Image'] == image_id, 'The active runner uses a different image')
    images.require(set(container['NetworkSettings']['Networks']) == {project + '-runner'},
                   'The active runner can reach a network outside its production broker network')
    for mount in container['Mounts']:
        images.require((mount['Type'] == 'volume' and mount.get('Name') in
                        {volumes['data'], volumes['runner'], volumes['tools']}) or
                       (mount['Type'] == 'tmpfs' and mount['Destination'] in ('/tmp', '/home/octomus')),
                       'A fixture or unexpected host mount appeared in the production runner')
    # A trusted assertion runs in the already broker-created container without changing its environment or mounts.
    run('docker', 'exec', identities[0], 'python3', '-c',
        'import errno,os,socket; assert all(not os.path.exists(p) for p in '
        '["/contract-driver","/provider.py","/provider.pem","/provider.key","/provider-status","/results"]); '
        's=socket.socket(); s.settimeout(2); assert s.connect_ex(("11.255.254.2",443)) '
        'not in (0,errno.ECONNREFUSED,errno.ECONNRESET); s.close()', capture=True)
    return container['Id']


def await_driver(name, project, volumes, image_id, directory):
    """Acknowledge each real session only after inspecting its actual container; fast clients cannot evade the check."""
    observed = {'codex': [], 'opencode': []}
    requests = {f'inspect-{backend}-{phase}': backend for backend in observed for phase in (1, 2)}
    deadline = time.monotonic() + 780
    try:
        while time.monotonic() < deadline:
            for request, backend in requests.items():
                if (directory / request).is_file() and not (directory / (request + '-complete')).exists():
                    observed[backend].append(inspect_runner(project, volumes, image_id))
                    (directory / (request + '-complete')).touch()
            state = json.loads(run('docker', 'inspect', '--format', '{{json .State}}', name, capture=True))
            if not state['Running']:
                images.require(state['ExitCode'] == 0 and not state['OOMKilled'], 'Production image driver failed')
                return observed
            time.sleep(0.2)
        raise RuntimeError('Production image driver exceeded its deadline')
    finally:
        run('docker', 'logs', name, check=False)


def accept(directory, output, source, version):
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    images.require(arch is not None, 'Run natively on an amd64 or arm64 Linux host')
    native = 'linux/' + arch
    daemon_arch = run('docker', 'info', '--format', '{{.OSType}}/{{.Architecture}}', capture=True)
    images.require(daemon_arch in (native, 'linux/' + platform.machine()), 'Docker daemon is not native to this runner')
    version_major = int(run('docker', 'version', '--format', '{{.Server.Version}}', capture=True).split('.')[0])
    images.require(version_major >= 28, 'Docker Engine 28+ is required for isolated gateway bridge mode')
    inputs = {name: images.metadata(directory, name, source, version) for name in images.IMAGES}
    driver = ROOT / 'bin/production-image-contract'
    images.require(driver.is_file(), 'Build bin/production-image-contract with make production-image-driver')
    project = 'img-' + uuid.uuid4().hex[:16]
    registry = project + '-registry'
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='octomus-images-') as temp:
        temp = Path(temp)
        temp.chmod(0o755)
        for name in ('status', 'results'):
            (temp / name).mkdir(mode=0o777)
            (temp / name).chmod(0o777)
        provider_files(temp)
        runner_configs(temp)
        volumes = {name: project + '-' + name for name in ('data', 'runner', 'tools', 'broker', 'egress-state')}
        compose_file = temp / 'compose.json'
        env = {**os.environ, 'OCTOMUS_GITHUB_REPO': 'fixture/production-images',
               'DOCKER_GID': str(Path('/var/run/docker.sock').stat().st_gid),
               'OCTOMUS_EGRESS_MODEL_HOSTS': PROVIDER_HOST, 'OCTOMUS_EGRESS_BUILD_HOSTS': ''}
        compose = ['docker', 'compose', '--project-name', project, '--project-directory', str(ROOT / 'deploy/docker'),
                   '-f', str(ROOT / 'deploy/docker/compose.yaml'), '-f', str(compose_file)]
        stack_defined = False
        try:
            run('docker', 'run', '-d', '--name', registry, '-p', '127.0.0.1::5000', 'registry:3', capture=True)
            port = run('docker', 'port', registry, '5000/tcp', capture=True).rsplit(':', 1)[1]
            registry_address = '127.0.0.1:' + port
            wait_for('local OCI registry', lambda: registry_ready(registry_address), 30)
            refs, docker_ids = {}, {}
            for name, metadata in inputs.items():
                destination = registry_address + '/' + name
                run('skopeo', 'copy', '--all', '--preserve-digests', '--dest-tls-verify=false',
                    'oci-archive:' + str(directory / metadata['archive']), 'docker://' + destination + ':candidate', timeout=600)
                refs[name] = destination + '@' + metadata['platforms'][native]['manifest_digest']
                run('docker', 'pull', '--platform', native, refs[name], timeout=600)
                actual = run('docker', 'image', 'inspect', '--format', '{{.Id}}', refs[name], capture=True)
                images.require(actual in metadata['platforms'][native].values(), 'Docker loaded different production image bytes')
                docker_ids[name] = actual
            env.update(OCTOMUS_IMAGE=refs['octomus-agent'], OCTOMUS_SANDBOX_IMAGE=refs['octomus-sandbox'])
            compose_file.write_text(json.dumps(overlay(project, refs, temp, volumes), indent=2))
            stack_defined = True
            for value in volumes.values():
                run('docker', 'volume', 'create', value, capture=True)
            # These directories come from the production image with uid 10001; volume copy-up preserves ownership.
            run('docker', 'run', '--rm', '--network', 'none', '--read-only', '--entrypoint', '/bin/sh',
                '-v', volumes['data'] + ':/var/lib/octomus/data', '-v', volumes['runner'] + ':/var/lib/octomus/runner',
                '-v', str(temp) + ':/input:ro', refs['octomus-agent'], '-euc',
                'mkdir -p /var/lib/octomus/runner/codex /var/lib/octomus/runner/opencode/config; '
                'cp /input/config.toml /var/lib/octomus/runner/codex/config.toml; '
                'cp /input/opencode.json /var/lib/octomus/runner/opencode/config/opencode.json', capture=True)
            run(*compose, 'up', '-d', '--no-build', '--pull', 'never', 'provider', 'egress', 'sandboxd', env=env, timeout=180)
            wait_for('synthetic TLS provider readiness', lambda: (temp / 'status/provider-port').is_file(), 30)
            provider = run(*compose, 'ps', '-q', 'provider', env=env, capture=True)
            inspect_topology(project, provider)
            broker = run(*compose, 'ps', '-q', 'sandboxd', env=env, capture=True)
            wait_for('production sandbox broker health', lambda: run(
                'docker', 'inspect', '--format', '{{.State.Health.Status}}', broker, capture=True) == 'healthy', 150)
            smoke_control_image(refs['octomus-agent'], project + '-http', version)
            run('docker', 'run', '-d', '--name', project + '-driver', '--network', 'none', '--read-only',
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true', '--tmpfs', '/tmp:size=64m',
                '--entrypoint', '/contract-driver', '-e', 'OCTOMUS_PRODUCTION_IMAGE_TEST=1',
                '-e', 'OCTOMUS_EXPECT_SANDBOX_ID=' + docker_ids['octomus-sandbox'],
                '-v', str(driver) + ':/contract-driver:ro', '-v', volumes['data'] + ':/var/lib/octomus/data',
                '-v', volumes['broker'] + ':/run/octomus:ro', '-v', str(temp / 'status') + ':/provider-status:ro',
                '-v', str(temp / 'results') + ':/results', refs['octomus-agent'],
                '-test.run=^TestProductionImageContracts$', '-test.v', '-test.timeout=12m', capture=True)
            runner_containers = await_driver(project + '-driver', project, volumes, docker_ids['octomus-sandbox'], temp / 'results')
            sandboxes = run('docker', 'ps', '-aq', '--filter', 'label=octomus.sandbox.instance=' + project, capture=True)
            leases = run('docker', 'exec', broker, '/bin/sh', '-ec',
                         'for lease in /run/octomus-egress/*.json; do [ ! -e "$lease" ] || echo "$lease"; done', capture=True)
            images.require(not sandboxes and not leases, 'Production acceptance left a sandbox or egress lease behind')
            receipt = {'schema': 1, 'platform': native, 'source_commit': source, 'passed': True,
                       'images': {name: {'digest': metadata['digest'], **metadata['platforms'][native]}
                                  for name, metadata in inputs.items()},
                       'docker_image_ids': docker_ids, 'runner_containers': runner_containers,
                       'cleanup': {'sandboxes': 0, 'leases': 0},
                       'contract': json.loads((temp / 'results/contract.json').read_text())}
            images.check_receipt(receipt, inputs)
            (output / ('production-acceptance-' + arch + '.json')).write_text(json.dumps(receipt, indent=2) + '\n')
            print('PASS: native ' + native + ' production OCI images accepted without rebuilding', flush=True)
        finally:
            if stack_defined:
                logs = run(*compose, 'logs', '--no-color', env=env, check=False, capture=True)
                driver_logs = run('docker', 'logs', project + '-driver', check=False, capture=True)
                (output / ('production-diagnostics-' + arch + '.log')).write_text(
                    (logs.stdout or '') + (logs.stderr or '') + (driver_logs.stdout or '') + (driver_logs.stderr or ''))
                run(*compose, 'down', '--volumes', '--remove-orphans', env=env, check=False, timeout=120)
            # Only resources carrying this invocation's random project name are touched.
            owned = run('docker', 'ps', '-aq', '--filter', 'label=octomus.sandbox.instance=' + project, capture=True)
            if owned:
                run('docker', 'rm', '-f', *owned.splitlines(), check=False, capture=True)
            run('docker', 'rm', '-f', registry, project + '-driver', project + '-http', check=False, capture=True)
            run('docker', 'volume', 'rm', *volumes.values(), check=False, capture=True)


def registry_ready(address):
    try:
        with urllib.request.urlopen('http://' + address + '/v2/', timeout=2) as response:
            return response.status == 200
    except (OSError, urllib.error.URLError):
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, default=ROOT / 'dist/images')
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/images')
    args = parser.parse_args()
    source = run('git', 'rev-parse', 'HEAD', capture=True)
    version = (ROOT / 'VERSION').read_text().strip()
    accept(args.directory.resolve(), args.output.resolve(), source, version)


if __name__ == '__main__':
    main()
