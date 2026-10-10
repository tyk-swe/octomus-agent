"""Verify retained OCI promotion without Docker, registry writes, or a signing identity.

The fixture archives contain content-addressed, two-platform image configurations and real
in-toto statement shapes. They are intentionally not executable images. Registry commands
are recorded by an in-memory peer so retries must reuse the accepted archive bytes.
"""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch


PROJECT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('release_images', PROJECT / 'scripts/release-images.py')
RELEASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RELEASE)

SOURCE = '1234567890abcdef1234567890abcdef12345678'
VERSION = '0.2.1'
TAG = 'v' + VERSION
OWNER = 'ghcr.io/example'
INDEX = 'application/vnd.oci.image.index.v1+json'
MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
CONFIG = 'application/vnd.oci.image.config.v1+json'
SPDX = 'https://spdx.dev/Document'
PROVENANCE = 'https://slsa.dev/provenance/v0.2'
CHECKS = ('non_root', 'no_capabilities', 'no_new_privileges', 'seccomp', 'read_only_image',
          'no_orchestrator_state', 'no_direct_egress', 'no_external_dns', 'no_host_route',
          'resource_limits', 'egress_gateway')


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


class OCIArchive:
    def __init__(self, image, *, platforms=('linux/amd64', 'linux/arm64'),
                 config_hook=None, statement_hook=None, index_hook=None):
        self.entries = {'oci-layout': encoded({'imageLayoutVersion': '1.0.0'})}
        self.attestation_blobs = []
        manifests, attestations = [], []
        for platform in platforms:
            system, architecture = platform.split('/')
            config = {'os': system, 'architecture': architecture,
                      'rootfs': {'type': 'layers', 'diff_ids': []},
                      'config': {'Labels': {'org.opencontainers.image.revision': SOURCE,
                                            'org.opencontainers.image.version': VERSION,
                                            'org.opencontainers.image.title': image}}}
            if config_hook:
                config_hook(config, platform)
            descriptor = self.blob({'schemaVersion': 2, 'mediaType': MANIFEST,
                                    'config': self.blob(config, CONFIG), 'layers': []}, MANIFEST)
            descriptor['platform'] = {'os': system, 'architecture': architecture}
            manifests.append(descriptor)
            layers = []
            for predicate_type, predicate in (
                    (SPDX, {'spdxVersion': 'SPDX-2.3', 'SPDXID': 'SPDXRef-DOCUMENT',
                            'name': image, 'documentNamespace': 'https://example.test/sbom/' + architecture}),
                    (PROVENANCE, {'buildType': 'https://mobyproject.org/buildkit@v1',
                                  'invocation': {'configSource': {'uri': 'https://example.test/source.git',
                                                                  'digest': {'sha1': SOURCE}}},
                                  'materials': [{'uri': 'https://example.test/source.git',
                                                 'digest': {'sha1': SOURCE}}]})):
                statement = {'_type': 'https://in-toto.io/Statement/v0.1', 'predicateType': predicate_type,
                             'subject': [{'name': image, 'digest': {'sha256': descriptor['digest'][7:]}}],
                             'predicate': predicate}
                if statement_hook:
                    statement_hook(statement, platform, predicate_type)
                layer = self.blob(statement, 'application/vnd.in-toto+json')
                self.attestation_blobs.append('blobs/sha256/' + layer['digest'][7:])
                layer['annotations'] = {'in-toto.io/predicate-type': predicate_type}
                layers.append(layer)
            attestation = self.blob({'schemaVersion': 2, 'mediaType': MANIFEST,
                                     'config': self.blob({'os': 'unknown', 'architecture': 'unknown'}, CONFIG),
                                     'layers': layers}, MANIFEST)
            attestation['platform'] = {'os': 'unknown', 'architecture': 'unknown'}
            attestation['annotations'] = {'vnd.docker.reference.type': 'attestation-manifest',
                                          'vnd.docker.reference.digest': descriptor['digest']}
            attestations.append(attestation)
        index = {'schemaVersion': 2, 'mediaType': INDEX, 'manifests': manifests + attestations}
        if index_hook:
            index_hook(index)
        self.index_bytes = encoded(index)
        self.index_descriptor = self.blob(index, INDEX)
        self.entries['index.json'] = encoded({'schemaVersion': 2, 'manifests': [self.index_descriptor]})

    def blob(self, value, media_type):
        data = encoded(value)
        digest = 'sha256:' + hashlib.sha256(data).hexdigest()
        self.entries['blobs/sha256/' + digest[7:]] = data
        return {'mediaType': media_type, 'digest': digest, 'size': len(data)}

    def write(self, path, *, extra_members=()):
        with tarfile.open(path, 'w', format=tarfile.USTAR_FORMAT) as archive:
            for name, data in [*self.entries.items(), *extra_members]:
                member = tarfile.TarInfo(name)
                member.size = len(data)
                member.mode = 0o644
                archive.addfile(member, io.BytesIO(data))


def sandbox_record(image_id, provider=False):
    return {'image_id': image_id, 'runtime': 'runc', 'runs': 1, 'oom': False, 'incomplete': False,
            'egress': {'allowed': {'provider.octomus.test:443': 1} if provider else {},
                       'denied': {}, 'failed': {}}}


def acceptance(platform, images):
    image_id = images['octomus-sandbox']['platforms'][platform]['image_id']
    return {'platform': platform, 'source_commit': SOURCE, 'passed': True,
            'images': {name: {'digest': image['digest'], **image['platforms'][platform]}
                       for name, image in images.items()},
            'docker_image_ids': {name: image['platforms'][platform]['image_id'] for name, image in images.items()},
            'runner_containers': {'codex': ['1' * 64, '2' * 64], 'opencode': ['3' * 64, '4' * 64]},
            'cleanup': {'sandboxes': 0, 'leases': 0},
            'contract': {'passed': True, 'broker_live': 0,
                         'contracts': {backend: [sandbox_record(image_id, True), sandbox_record(image_id, True)]
                                       for backend in ('codex', 'opencode')},
                         'containment': {'checks': [{'id': name, 'passed': True} for name in CHECKS]},
                         'containment_sandbox': sandbox_record(image_id),
                         'verification': sandbox_record(image_id)}}


class Registry:
    """Only the immutable-archive promotion protocol is accepted by this peer."""
    def __init__(self, directory, archives):
        self.directory, self.archives = directory, archives
        self.images, self.calls = {}, []
        self.copy_failures = set()
        self.copy_payloads = {}
        self.inspect_error = None

    def run(self, args, *, capture_output=False, check=False):
        self.calls.append(args)
        if args[:3] == ['skopeo', 'inspect', '--raw']:
            ref = args[3]
            if self.inspect_error:
                result = subprocess.CompletedProcess(args, 1, b'', self.inspect_error)
            elif ref in self.images:
                result = subprocess.CompletedProcess(args, 0, self.images[ref], b'')
            else:
                result = subprocess.CompletedProcess(args, 1, b'', b'manifest unknown')
        elif args[:4] == ['skopeo', 'copy', '--all', '--preserve-digests']:
            archive, ref = args[4:]
            image = Path(archive.removeprefix('oci-archive:')).name.removesuffix('.oci.tar')
            if archive != 'oci-archive:' + str(self.directory / (image + '.oci.tar')):
                raise AssertionError('Promotion did not reuse the retained archive: ' + repr(args))
            if image in self.copy_failures:
                self.copy_failures.remove(image)
                result = subprocess.CompletedProcess(args, 1, b'', b'synthetic copy interruption')
            else:
                self.images[ref] = self.copy_payloads.get(image, self.archives[image].index_bytes)
                result = subprocess.CompletedProcess(args, 0, b'', b'')
        elif args[:3] == ['cosign', 'sign', '--yes']:
            if '@sha256:' not in args[3]:
                raise AssertionError('Signing must use an immutable digest')
            result = subprocess.CompletedProcess(args, 0, b'', b'')
        else:
            raise AssertionError('Unexpected registry/build command: ' + repr(args))
        if check and result.returncode:
            raise subprocess.CalledProcessError(result.returncode, args, output=result.stdout, stderr=result.stderr)
        return result


class ReleaseImageTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='octomus-release-images-')
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.archives, self.images = {}, {}
        for image in ('octomus-agent', 'octomus-sandbox'):
            archive = OCIArchive(image)
            archive.write(self.directory / (image + '.oci.tar'))
            self.archives[image] = archive
            metadata = RELEASE.metadata(self.directory, image, SOURCE, VERSION)
            self.images[image] = metadata
            self.write_json(image + '.json', metadata)
        self.receipts = [acceptance(platform, self.images) for platform in ('linux/amd64', 'linux/arm64')]
        for receipt in self.receipts:
            self.write_json('production-acceptance-' + receipt['platform'].split('/')[1] + '.json', receipt)
        self.release = RELEASE.manifest(self.directory, SOURCE, VERSION, TAG)
        self.write_json('release-images.json', self.release)

    def write_json(self, name, value):
        (self.directory / name).write_text(json.dumps(value) + '\n')

    def test_valid_two_platform_archive_and_retained_release(self):
        retained = RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)
        self.assertEqual(retained, self.release)
        self.assertEqual(set(retained['images']), {'octomus-agent', 'octomus-sandbox'})
        for image, metadata in retained['images'].items():
            self.assertEqual(metadata['digest'], self.archives[image].index_descriptor['digest'])
            self.assertEqual(metadata['sha256'], hashlib.sha256((self.directory / metadata['archive']).read_bytes()).hexdigest())
            self.assertEqual(set(metadata['platforms']), {'linux/amd64', 'linux/arm64'})
            self.assertNotEqual(metadata['platforms']['linux/amd64']['image_id'],
                                metadata['platforms']['linux/arm64']['image_id'])

    def test_missing_or_changed_archive_is_rejected_before_promotion(self):
        path = self.directory / 'octomus-agent.oci.tar'
        saved = path.read_bytes()
        path.unlink()
        with self.assertRaises(FileNotFoundError):
            RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)
        # Still a readable tar with identical manifests, but no longer the archive that passed acceptance.
        path.write_bytes(saved + b'changed retained bytes')
        with self.assertRaisesRegex(ValueError, 'input changed'):
            RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)

    def test_missing_metadata_or_native_receipt_is_rejected(self):
        for name in ('octomus-agent.json', 'production-acceptance-arm64.json'):
            with self.subTest(name=name):
                path = self.directory / name
                saved = path.read_bytes()
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    RELEASE.manifest(self.directory, SOURCE, VERSION, TAG)
                path.write_bytes(saved)

    def test_source_version_and_image_labels_are_required_on_both_platforms(self):
        for label, wrong in (('revision', 'f' * 40), ('version', '9.9.9'), ('title', 'other-image')):
            with self.subTest(label=label):
                archive = OCIArchive('octomus-agent', config_hook=lambda value, platform:
                                     value['config']['Labels'].__setitem__('org.opencontainers.image.' + label, wrong))
                archive.write(self.directory / 'octomus-agent.oci.tar')
                with self.assertRaisesRegex(ValueError, 'labels'):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)
        archive = OCIArchive('octomus-agent', config_hook=lambda value, platform:
                             value['config']['Labels'].clear() if platform == 'linux/arm64' else None)
        archive.write(self.directory / 'octomus-agent.oci.tar')
        with self.assertRaisesRegex(ValueError, 'identity differs'):
            RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_mutable_or_wrong_release_identity_is_rejected(self):
        for source, version, tag in (('main', VERSION, TAG), ('f' * 40, VERSION, TAG),
                                     (SOURCE, '9.9.9', 'v9.9.9'), (SOURCE, VERSION, 'v9.9.9')):
            with self.subTest(source=source, version=version, tag=tag):
                with self.assertRaises(ValueError):
                    RELEASE.verify_release(self.directory, source, version, tag)
        with self.assertRaisesRegex(ValueError, 'full Git SHA'):
            RELEASE.metadata(self.directory, 'octomus-agent', 'main', VERSION)

    def test_extra_missing_or_duplicate_platform_is_rejected(self):
        for platforms in (('linux/amd64', 'linux/arm64', 'linux/riscv64'), ('linux/amd64',),
                          ('linux/amd64', 'linux/arm64', 'linux/arm64')):
            with self.subTest(platforms=platforms):
                OCIArchive('octomus-agent', platforms=platforms).write(self.directory / 'octomus-agent.oci.tar')
                with self.assertRaises(ValueError):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_missing_or_corrupt_attestation_payload_is_rejected(self):
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt):
                archive = OCIArchive('octomus-agent')
                target = archive.attestation_blobs[0]
                if corrupt:
                    archive.entries[target] = encoded({'changed': True})
                else:
                    del archive.entries[target]
                archive.write(self.directory / 'octomus-agent.oci.tar')
                with self.assertRaisesRegex(ValueError, 'OCI document'):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_attestation_statement_must_bind_subject_type_and_predicate(self):
        mutations = {
            'unnamed OCI export with empty subjects': lambda value: value.__setitem__('subject', []),
            'missing subjects': lambda value: value.pop('subject'),
            'wrong subject': lambda value: value['subject'][0]['digest'].__setitem__('sha256', '0' * 64),
            'wrong statement type': lambda value: value.__setitem__('_type', 'unrecognized'),
            'mismatched predicate annotation': lambda value: value.__setitem__('predicateType', 'unrecognized'),
            'empty predicate': lambda value: value.__setitem__('predicate', {}),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                archive = OCIArchive('octomus-agent', statement_hook=lambda value, platform, kind: mutation(value))
                archive.write(self.directory / 'octomus-agent.oci.tar')
                with self.assertRaises(ValueError):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_named_oci_export_subjects_bind_to_each_native_manifest(self):
        # BuildKit derives in-toto subjects from the OCI exporter's image name. A
        # source-specific local tag supplies names without publishing to a registry.
        def named_subject(value, platform, kind):
            value['subject'][0]['name'] = ('pkg:docker/octomus-acceptance/octomus-agent@' + SOURCE +
                                          '?platform=' + platform.replace('/', '%2F'))
        OCIArchive('octomus-agent', statement_hook=named_subject).write(
            self.directory / 'octomus-agent.oci.tar')
        metadata = RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)
        self.assertEqual(set(metadata['platforms']), {'linux/amd64', 'linux/arm64'})

    def test_missing_or_unrelated_attestation_is_rejected(self):
        mutations = {
            'missing arm64 evidence': lambda index: index['manifests'].pop(),
            'unrelated subject': lambda index: index['manifests'][-1]['annotations'].__setitem__(
                'vnd.docker.reference.digest', 'sha256:' + '0' * 64),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                OCIArchive('octomus-agent', index_hook=mutation).write(self.directory / 'octomus-agent.oci.tar')
                with self.assertRaises(ValueError):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_duplicate_and_unsafe_archive_paths_are_rejected(self):
        archive = self.archives['octomus-agent']
        for name in ('index.json', './index.json', '../index.json', '/index.json'):
            with self.subTest(name=name):
                archive.write(self.directory / 'octomus-agent.oci.tar',
                              extra_members=[(name, archive.entries['index.json'])])
                with self.assertRaisesRegex(ValueError, 'OCI archive member'):
                    RELEASE.metadata(self.directory, 'octomus-agent', SOURCE, VERSION)

    def test_extra_release_image_is_neither_verified_nor_promoted(self):
        release = copy.deepcopy(self.release)
        release['images']['untested-image'] = copy.deepcopy(release['images']['octomus-agent'])
        self.write_json('release-images.json', release)
        with self.assertRaisesRegex(ValueError, 'exactly the two production images'):
            RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)
        with patch.object(RELEASE.subprocess, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'extra release images'):
                RELEASE.promote(self.directory, release, OWNER)
            run.assert_not_called()

    def test_each_native_receipt_must_match_all_accepted_image_identities(self):
        for platform_index in range(2):
            for image in ('octomus-agent', 'octomus-sandbox'):
                for field in ('digest', 'manifest_digest', 'image_id'):
                    with self.subTest(platform=platform_index, image=image, field=field):
                        receipt = copy.deepcopy(self.receipts[platform_index])
                        receipt['images'][image][field] = 'sha256:' + '0' * 64
                        with self.assertRaisesRegex(ValueError, 'image identity mismatch'):
                            RELEASE.check_receipt(receipt, self.images)

    def test_both_native_platforms_and_the_same_source_are_required(self):
        release = copy.deepcopy(self.release)
        release['acceptance'][1] = copy.deepcopy(release['acceptance'][0])
        self.write_json('release-images.json', release)
        with self.assertRaisesRegex(ValueError, 'Both native architecture'):
            RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)
        receipt = copy.deepcopy(self.receipts[0])
        receipt['source_commit'] = 'f' * 40
        with self.assertRaisesRegex(ValueError, 'source commit mismatch'):
            RELEASE.check_receipt(receipt, self.images)

    def test_containerd_native_manifest_ids_are_accepted_and_bind_runtime_records(self):
        for receipt in copy.deepcopy(self.receipts):
            platform = receipt['platform']
            for name, image in self.images.items():
                receipt['docker_image_ids'][name] = image['platforms'][platform]['manifest_digest']
            sandbox_id = receipt['docker_image_ids']['octomus-sandbox']
            for runs in receipt['contract']['contracts'].values():
                for record in runs:
                    record['image_id'] = sandbox_id
            receipt['contract']['containment_sandbox']['image_id'] = sandbox_id
            receipt['contract']['verification']['image_id'] = sandbox_id
            RELEASE.check_receipt(receipt, self.images)
            receipt['contract']['verification']['image_id'] = self.images['octomus-sandbox']['platforms'][platform]['image_id']
            with self.assertRaisesRegex(ValueError, 'verification'):
                RELEASE.check_receipt(receipt, self.images)

    def test_docker_identity_must_be_the_exact_native_config_or_manifest(self):
        for image in ('octomus-agent', 'octomus-sandbox'):
            for identity in ('sha256:' + 'f' * 64, self.images[image]['digest'],
                             self.images[image]['platforms']['linux/arm64']['image_id'],
                             self.images[image]['platforms']['linux/arm64']['manifest_digest']):
                with self.subTest(image=image, identity=identity):
                    receipt = copy.deepcopy(self.receipts[0])
                    receipt['docker_image_ids'][image] = identity
                    with self.assertRaisesRegex(ValueError, 'Docker'):
                        RELEASE.check_receipt(receipt, self.images)

    def test_retained_resume_evidence_requires_four_distinct_container_ids(self):
        mutations = {
            'missing identities': lambda value: value.pop('runner_containers'),
            'missing backend': lambda value: value['runner_containers'].pop('opencode'),
            'one run': lambda value: value['runner_containers']['codex'].pop(),
            'duplicate within backend': lambda value: value['runner_containers']['codex'].__setitem__(1, '1' * 64),
            'duplicate between backends': lambda value: value['runner_containers']['opencode'].__setitem__(1, '1' * 64),
            'empty identity': lambda value: value['runner_containers']['codex'].__setitem__(0, ''),
            'short identity': lambda value: value['runner_containers']['codex'].__setitem__(0, 'a' * 12),
            'not hexadecimal': lambda value: value['runner_containers']['codex'].__setitem__(0, 'x' * 64),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                receipt = copy.deepcopy(self.receipts[0])
                mutation(receipt)
                with self.assertRaises(ValueError):
                    RELEASE.check_receipt(receipt, self.images)

    def test_receipts_require_complete_runtime_and_cleanup_evidence(self):
        mutations = {
            'failed acceptance': lambda value: value.__setitem__('passed', False),
            'live sandbox': lambda value: value['cleanup'].__setitem__('sandboxes', 1),
            'remaining lease': lambda value: value['cleanup'].__setitem__('leases', 1),
            'live broker': lambda value: value['contract'].__setitem__('broker_live', 1),
            'missing backend': lambda value: value['contract']['contracts'].pop('opencode'),
            'missing reconnect': lambda value: value['contract']['contracts']['codex'].pop(),
            'no provider CONNECT': lambda value: value['contract']['contracts']['codex'][0]['egress'].__setitem__('allowed', {}),
            'incomplete runner': lambda value: value['contract']['contracts']['codex'][0].__setitem__('incomplete', True),
            'runner OOM': lambda value: value['contract']['contracts']['opencode'][1].__setitem__('oom', True),
            'zero runs': lambda value: value['contract']['contracts']['opencode'][0].__setitem__('runs', 0),
            'failed containment': lambda value: value['contract']['containment']['checks'][0].__setitem__('passed', False),
            'duplicate check': lambda value: value['contract']['containment']['checks'].__setitem__(
                1, copy.deepcopy(value['contract']['containment']['checks'][0])),
            'missing probe identity': lambda value: value['contract'].pop('containment_sandbox'),
            'probe OOM': lambda value: value['contract']['containment_sandbox'].__setitem__('oom', True),
            'incomplete verification': lambda value: value['contract']['verification'].__setitem__('incomplete', True),
            'verification OOM': lambda value: value['contract']['verification'].__setitem__('oom', True),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                receipt = copy.deepcopy(self.receipts[0])
                mutation(receipt)
                with self.assertRaises(ValueError):
                    RELEASE.check_receipt(receipt, self.images)

    def test_unknown_or_wrongly_typed_runtime_evidence_is_rejected(self):
        for name in ('codex', 'opencode', 'containment_sandbox', 'verification'):
            for field, bad_values in (('incomplete', (None, 0, '')), ('oom', (None, 0, '')),
                                      ('runs', (None, True, 1.0, '1'))):
                for bad in bad_values:
                    with self.subTest(record=name, field=field, value=bad):
                        receipt = copy.deepcopy(self.receipts[0])
                        if name in ('codex', 'opencode'):
                            record = receipt['contract']['contracts'][name][0]
                        else:
                            record = receipt['contract'][name]
                        record[field] = bad
                        with self.assertRaises(ValueError):
                            RELEASE.check_receipt(receipt, self.images)

    def test_promotion_reuses_retained_digests_and_identical_tags_are_idempotent(self):
        registry = Registry(self.directory, self.archives)
        with patch.object(RELEASE.subprocess, 'run', side_effect=registry.run):
            RELEASE.promote(self.directory, self.release, OWNER)
            RELEASE.promote(self.directory, self.release, OWNER)
        copies = [args for args in registry.calls if args[:2] == ['skopeo', 'copy']]
        self.assertEqual(len(copies), 2, registry.calls)
        for image in ('octomus-agent', 'octomus-sandbox'):
            ref = 'docker://' + OWNER + '/' + image + ':' + VERSION
            self.assertEqual(registry.images[ref], self.archives[image].index_bytes)
            self.assertIn(['skopeo', 'copy', '--all', '--preserve-digests',
                           'oci-archive:' + str(self.directory / (image + '.oci.tar')), ref], copies)
        signs = [args for args in registry.calls if args[0] == 'cosign']
        self.assertEqual(len(signs), 4)
        self.assertEqual({args[3] for args in signs},
                         {OWNER + '/' + name + '@' + item['digest'] for name, item in self.images.items()})

    def test_existing_different_version_tag_is_never_replaced_or_signed(self):
        registry = Registry(self.directory, self.archives)
        registry.images['docker://' + OWNER + '/octomus-agent:' + VERSION] = b'a different image manifest'
        with patch.object(RELEASE.subprocess, 'run', side_effect=registry.run):
            with self.assertRaisesRegex(ValueError, 'Refusing to replace a version tag'):
                RELEASE.promote(self.directory, self.release, OWNER)
        self.assertEqual(len(registry.calls), 1)

    def test_successful_copy_must_still_resolve_to_the_accepted_digest_before_signing(self):
        registry = Registry(self.directory, self.archives)
        registry.copy_payloads['octomus-agent'] = b'a changed image index'
        with patch.object(RELEASE.subprocess, 'run', side_effect=registry.run):
            with self.assertRaisesRegex(ValueError, 'Promoted digest changed'):
                RELEASE.promote(self.directory, self.release, OWNER)
        self.assertEqual([args[1] for args in registry.calls], ['inspect', 'copy', 'inspect'])

    def test_inspection_error_cannot_be_treated_as_a_missing_tag(self):
        for error in (b'authentication required', b'too many requests', b'TLS certificate verification failed',
                      b'error initializing source: lookup ghcr.io: host not found',
                      b'opening trusted certificate file: ca.pem not found'):
            with self.subTest(error=error):
                registry = Registry(self.directory, self.archives)
                registry.inspect_error = error
                with patch.object(RELEASE.subprocess, 'run', side_effect=registry.run):
                    with self.assertRaisesRegex(ValueError, 'Cannot check existing registry image'):
                        RELEASE.promote(self.directory, self.release, OWNER)
                self.assertEqual(len(registry.calls), 1)

    def test_partial_promotion_retry_does_not_rebuild_or_replace_the_first_image(self):
        registry = Registry(self.directory, self.archives)
        registry.copy_failures.add('octomus-sandbox')
        with patch.object(RELEASE.subprocess, 'run', side_effect=registry.run):
            with self.assertRaises(subprocess.CalledProcessError):
                RELEASE.promote(self.directory, self.release, OWNER)
            # Retry loads and validates the same durable receipt and OCI archives.
            retained = RELEASE.verify_release(self.directory, SOURCE, VERSION, TAG)
            RELEASE.promote(self.directory, retained, OWNER)
        copies = [args for args in registry.calls if args[:2] == ['skopeo', 'copy']]
        agent_copies = [args for args in copies if args[-1].endswith('/octomus-agent:' + VERSION)]
        sandbox_copies = [args for args in copies if args[-1].endswith('/octomus-sandbox:' + VERSION)]
        self.assertEqual(len(agent_copies), 1, registry.calls)
        self.assertEqual(len(sandbox_copies), 2, registry.calls)
        self.assertEqual(sandbox_copies[0], sandbox_copies[1])


if __name__ == '__main__':
    unittest.main()
