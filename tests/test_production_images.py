"""Exercise image-acceptance host checks with controlled Docker responses, without a daemon."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('production_images', PROJECT_ROOT / 'tests/production_images.py')
PRODUCTION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PRODUCTION)

PROJECT = 'img-unit-12345678'
IMAGE_ID = 'sha256:' + 'a' * 64
CONTAINER_ID = 'b' * 64
VOLUMES = {name: PROJECT + '-' + name for name in ('data', 'runner', 'tools', 'broker', 'egress-state')}


def runner_container():
    return {'Id': CONTAINER_ID, 'Image': IMAGE_ID,
            'NetworkSettings': {'Networks': {PROJECT + '-runner': {}}},
            'Mounts': [{'Type': 'volume', 'Name': VOLUMES['data'], 'Destination': '/workspace'},
                       {'Type': 'volume', 'Name': VOLUMES['runner'], 'Destination': '/home/octomus/.codex'},
                       {'Type': 'volume', 'Name': VOLUMES['tools'], 'Destination': '/opt/octomus-tools'},
                       {'Type': 'tmpfs', 'Destination': '/tmp'}]}


class ProductionImageHostTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix='octomus-production-host-')
        self.addCleanup(directory.cleanup)
        self.directory = Path(directory.name)

    def test_overlay_scopes_volumes_networks_and_fixture_mounts_to_the_invocation(self):
        refs = {'octomus-agent': 'local/agent@' + IMAGE_ID, 'octomus-sandbox': 'local/sandbox@' + IMAGE_ID}
        overlay = PRODUCTION.overlay(PROJECT, refs, self.directory, VOLUMES)
        self.assertEqual(overlay['volumes'], {name: {'name': value, 'external': True} for name, value in VOLUMES.items()})
        broker = overlay['services']['sandboxd']
        self.assertEqual(broker['environment']['OCTOMUS_SANDBOX_INSTANCE'], PROJECT)
        for kind in ('DATA', 'RUNNER', 'TOOLS'):
            self.assertEqual(broker['environment']['OCTOMUS_SANDBOX_' + kind + '_VOLUME'], VOLUMES[kind.lower()])
        self.assertNotIn('network_mode', broker)
        self.assertNotIn('networks', broker)
        self.assertEqual(broker['volumes'], [{'type': 'bind', 'source': str(self.directory / 'provider-ca.pem'),
                                             'target': '/run/acceptance/provider-ca.pem', 'read_only': True}])
        provider = overlay['services']['provider']
        self.assertEqual(provider['image'], refs['octomus-sandbox'])
        self.assertEqual(provider['pull_policy'], 'never')
        self.assertEqual(set(provider['networks']), {'provider'})
        self.assertNotIn('ports', provider)
        self.assertNotIn('network_mode', provider)
        self.assertEqual(overlay['networks']['provider']['name'], PROJECT + '-provider')
        self.assertIs(overlay['networks']['provider']['internal'], True)
        for kind in ('runner', 'verify'):
            self.assertEqual(overlay['networks']['sandbox-' + kind]['name'], PROJECT + '-' + kind)
            self.assertEqual(broker['environment']['OCTOMUS_SANDBOX_' + kind.upper() + '_NETWORK'], PROJECT + '-' + kind)

    def topology(self):
        network = {'Internal': True, 'EnableIPv6': False,
                   'Options': {'com.docker.network.bridge.gateway_mode_ipv4': 'isolated'}}
        provider = {'NetworkSettings': {'Networks': {PROJECT + '-provider': {}}, 'Ports': {'443/tcp': None}},
                    'HostConfig': {'PortBindings': {}}}
        return network, provider

    def inspect_topology(self, network, provider):
        def run(*args, **kwargs):
            self.assertIs(kwargs['capture'], True)
            if args == ('docker', 'network', 'inspect', PROJECT + '-provider'):
                return json.dumps([network])
            if args == ('docker', 'inspect', 'provider-container'):
                return json.dumps([provider])
            self.fail('Unexpected topology command: ' + repr(args))
        with patch.object(PRODUCTION, 'run', side_effect=run):
            PRODUCTION.inspect_topology(PROJECT, 'provider-container')

    def test_private_provider_topology_is_accepted(self):
        self.inspect_topology(*self.topology())

    def test_provider_exposure_or_nonisolated_topology_is_rejected(self):
        mutations = {
            'external network': lambda net, ctr: net.__setitem__('Internal', False),
            'IPv6 enabled': lambda net, ctr: net.__setitem__('EnableIPv6', True),
            'routed bridge': lambda net, ctr: net['Options'].__setitem__('com.docker.network.bridge.gateway_mode_ipv4', 'routed'),
            'missing bridge mode': lambda net, ctr: net['Options'].clear(),
            'additional network': lambda net, ctr: ctr['NetworkSettings']['Networks'].__setitem__('bridge', {}),
            'configured published port': lambda net, ctr: ctr['HostConfig']['PortBindings'].__setitem__(
                '443/tcp', [{'HostIp': '127.0.0.1', 'HostPort': '4443'}]),
            'actual published port': lambda net, ctr: ctr['NetworkSettings']['Ports'].__setitem__(
                '443/tcp', [{'HostIp': '0.0.0.0', 'HostPort': '4443'}]),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                network, provider = self.topology()
                mutation(network, provider)
                with self.assertRaises(ValueError):
                    self.inspect_topology(network, provider)

    def inspect_runner(self, container, *, active=None, probe_error=False):
        calls = []

        def run(*args, **kwargs):
            calls.append(args)
            self.assertIs(kwargs['capture'], True)
            if args[:3] == ('docker', 'ps', '-q'):
                self.assertIn('label=octomus.sandbox.instance=' + PROJECT, args)
                self.assertIn('label=octomus.sandbox.kind=runner', args)
                return CONTAINER_ID[:12] if active is None else active
            if args == ('docker', 'inspect', CONTAINER_ID[:12]):
                return json.dumps([container])
            if args[:4] == ('docker', 'exec', CONTAINER_ID[:12], 'python3'):
                self.assertIn(PRODUCTION.PROVIDER_IP, args[-1])
                self.assertIn('/contract-driver', args[-1])
                if probe_error:
                    raise RuntimeError('synthetic direct-access or fixture-presence assertion failed')
                return ''
            self.fail('Unexpected runner inspection command: ' + repr(args))
        with patch.object(PRODUCTION, 'run', side_effect=run):
            result = PRODUCTION.inspect_runner(PROJECT, VOLUMES, IMAGE_ID)
        return result, calls

    def test_runner_identity_is_retained_only_after_direct_access_assertions_pass(self):
        identity, calls = self.inspect_runner(runner_container())
        self.assertEqual(identity, CONTAINER_ID)
        self.assertEqual([args[1] for args in calls], ['ps', 'inspect', 'exec'])
        with self.assertRaisesRegex(RuntimeError, 'assertion failed'):
            self.inspect_runner(runner_container(), probe_error=True)

    def test_unexpected_runner_image_network_mount_or_active_count_is_rejected(self):
        mutations = {
            'wrong image': lambda ctr: ctr.__setitem__('Image', 'sha256:' + 'f' * 64),
            'provider network': lambda ctr: ctr['NetworkSettings']['Networks'].__setitem__(PROJECT + '-provider', {}),
            'wrong network': lambda ctr: ctr['NetworkSettings'].__setitem__('Networks', {'bridge': {}}),
            'fixture bind': lambda ctr: ctr['Mounts'].append({'Type': 'bind', 'Source': '/tmp/provider.py', 'Destination': '/provider.py'}),
            'foreign volume': lambda ctr: ctr['Mounts'][0].__setitem__('Name', 'another-invocation-data'),
            'unexpected tmpfs': lambda ctr: ctr['Mounts'].append({'Type': 'tmpfs', 'Destination': '/provider-status'}),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                container = runner_container()
                mutation(container)
                with self.assertRaises(ValueError):
                    self.inspect_runner(container)
        for active in ('', 'first\nsecond'):
            with self.subTest(active=active):
                with self.assertRaisesRegex(ValueError, 'one active production runner'):
                    self.inspect_runner(runner_container(), active=active)

    def test_each_session_waits_for_host_inspection_and_is_acknowledged_once(self):
        phases = ['inspect-codex-1', 'inspect-codex-2', 'inspect-opencode-1', 'inspect-opencode-2']
        identities = [str(index) * 64 for index in range(1, 5)]
        (self.directory / phases[0]).touch()
        inspected = []
        states = 0

        def inspect(project, volumes, image_id):
            self.assertEqual((project, volumes, image_id), (PROJECT, VOLUMES, IMAGE_ID))
            phase = phases[len(inspected)]
            self.assertFalse((self.directory / (phase + '-complete')).exists())
            inspected.append(phase)
            return identities[len(inspected) - 1]

        def run(*args, **kwargs):
            nonlocal states
            if args[:3] == ('docker', 'inspect', '--format'):
                self.assertEqual(args[-1], 'driver')
                self.assertTrue((self.directory / (phases[states] + '-complete')).is_file())
                states += 1
                if states < len(phases):
                    (self.directory / phases[states]).touch()
                return json.dumps({'Running': states < len(phases), 'ExitCode': 0, 'OOMKilled': False})
            self.assertEqual(args, ('docker', 'logs', 'driver'))
            self.assertIs(kwargs['check'], False)
            return None

        with patch.object(PRODUCTION, 'inspect_runner', side_effect=inspect), \
                patch.object(PRODUCTION, 'run', side_effect=run), patch.object(PRODUCTION.time, 'sleep'):
            result = PRODUCTION.await_driver('driver', PROJECT, VOLUMES, IMAGE_ID, self.directory)
        self.assertEqual(inspected, phases)
        self.assertEqual(result, {'codex': identities[:2], 'opencode': identities[2:]})

    def test_failed_inspection_never_acknowledges_the_session(self):
        (self.directory / 'inspect-codex-1').touch()
        with patch.object(PRODUCTION, 'inspect_runner', side_effect=ValueError('unsafe runner')), \
                patch.object(PRODUCTION, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'unsafe runner'):
                PRODUCTION.await_driver('driver', PROJECT, VOLUMES, IMAGE_ID, self.directory)
        self.assertFalse((self.directory / 'inspect-codex-1-complete').exists())
        run.assert_called_once_with('docker', 'logs', 'driver', check=False)

    def test_failed_or_oom_killed_driver_cannot_produce_acceptance(self):
        for state in ({'Running': False, 'ExitCode': 1, 'OOMKilled': False},
                      {'Running': False, 'ExitCode': 0, 'OOMKilled': True}):
            with self.subTest(state=state):
                with patch.object(PRODUCTION, 'run', side_effect=[json.dumps(state), None]):
                    with self.assertRaisesRegex(ValueError, 'driver failed'):
                        PRODUCTION.await_driver('driver', PROJECT, VOLUMES, IMAGE_ID, self.directory)

    def test_driver_timeout_is_bounded_and_keeps_diagnostics(self):
        with patch.object(PRODUCTION.time, 'monotonic', side_effect=[0, 781]), \
                patch.object(PRODUCTION, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, 'exceeded its deadline'):
                PRODUCTION.await_driver('driver', PROJECT, VOLUMES, IMAGE_ID, self.directory)
        run.assert_called_once_with('docker', 'logs', 'driver', check=False)


if __name__ == '__main__':
    unittest.main()
