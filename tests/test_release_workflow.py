"""Exercise immutable release staging and recovery with a stateful offline GitHub peer.

Real OCI inspection and native-acceptance validation run against the small archives from
test_release_images. Packages are synthetic bytes: this suite verifies their retention,
checksums and publication order, while distribution tests exercise executable packages.
Every external command is refused unless the peer explicitly implements it.
"""
import base64
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

from test_release_images import OCIArchive, SOURCE, TAG, VERSION, acceptance


PROJECT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('release_workflow', PROJECT / 'scripts/release.py')
RELEASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RELEASE)
ORIGINAL_RUN = 701
RETRY_RUN = 999


class GitHubPeer(RELEASE.GitHub):
    """Model draft discovery, artifact retention and immutable asset uploads, without gh."""

    def __init__(self, artifacts):
        super().__init__('fixture/releases')
        self.artifacts = {ORIGINAL_RUN: artifacts}
        self.release_record = None
        self.asset_records, self.asset_bytes = {}, {}
        self.events, self.publications = [], []
        self.source = SOURCE
        self.source_available = True
        self.fail_upload_at = None
        self.upload_attempts = 0
        self.leave_starter = False
        self.after_upload = None
        self.next_asset = 1000

    def api(self, path, *, method='GET', data=None, missing=False):
        self.events.append(('api', method, path, copy.deepcopy(data)))
        parsed = urlsplit(path)
        if method == 'GET' and parsed.path.startswith(('git/', 'contents/')):
            if not self.source_available:
                raise AssertionError('This operation must not read the tag or VERSION')
            if parsed.path == 'git/ref/tags/' + TAG:
                return {'object': {'type': 'commit', 'sha': self.source}}
            if path == 'contents/VERSION?ref=' + self.source:
                content = (VERSION + '\n').encode()
                return {'type': 'file', 'encoding': 'base64', 'size': len(content),
                        'content': base64.b64encode(content).decode()}
        if method == 'POST' and path == 'releases/generate-notes':
            return {'body': 'Generated release notes.'}
        if method == 'GET' and parsed.path == 'releases/tags/' + TAG:
            if self.release_record is not None and not self.release_record['draft']:
                return copy.deepcopy(self.release_record)
            if missing:
                return None
        if method == 'GET' and parsed.path in ('releases', 'releases/41/assets'):
            values = ([self.release_record] if self.release_record is not None else [])
            if parsed.path.endswith('/assets'):
                values = list(self.asset_records.values())
            query = parse_qs(parsed.query)
            if query.get('per_page') != ['100']:
                raise AssertionError('Inventory must use bounded pagination')
            offset = (int(query['page'][0]) - 1) * 100
            return copy.deepcopy(values[offset:offset + 100])
        if method == 'PATCH' and path == 'releases/41':
            self.release_record.update(copy.deepcopy(data))
            if data.get('draft') is False:
                self.publications.append((copy.deepcopy(self.release_record),
                                          copy.deepcopy(self.asset_records), dict(self.asset_bytes)))
            return copy.deepcopy(self.release_record)
        if method == 'DELETE' and path.startswith('releases/assets/'):
            identity = int(path.rsplit('/', 1)[1])
            name = next(name for name, item in self.asset_records.items() if item['id'] == identity)
            del self.asset_records[name]
            self.asset_bytes.pop(name, None)
            return None
        raise AssertionError('Unexpected GitHub API operation: ' + repr((method, path, data)))

    def command(self, args):
        self.events.append(('command', tuple(args)))
        if args[:2] == ['run', 'download']:
            run_id = int(args[2])
            if run_id not in self.artifacts:
                raise subprocess.CalledProcessError(1, ['gh', *args])
            directory = Path(args[args.index('--dir') + 1])
            for artifact, files in self.artifacts[run_id].items():
                target = directory / artifact
                target.mkdir(parents=True)
                for name, content in files.items():
                    (target / name).write_bytes(content)
            return
        if args[:2] == ['release', 'create']:
            if self.release_record is not None or '--draft' not in args or '--verify-tag' not in args:
                raise AssertionError('A new release must be a verified-tag draft')
            self.release_record = {'id': 41, 'tag_name': args[2], 'draft': True,
                                   'body': Path(args[args.index('--notes-file') + 1]).read_text()}
            self.events.append(('created', copy.deepcopy(self.release_record)))
            return
        if args[:2] == ['release', 'upload']:
            if len(args) != 4:
                raise AssertionError('Release uploads must never clobber existing assets')
            path = Path(args[3])
            if path.name in self.asset_records:
                raise AssertionError('Uploaded asset was replaced: ' + path.name)
            self.upload_attempts += 1
            if self.upload_attempts == self.fail_upload_at:
                if self.leave_starter:
                    self.add_asset(path.name, b'', state='starter')
                raise subprocess.CalledProcessError(1, ['gh', *args])
            self.add_asset(path.name, path.read_bytes())
            if self.after_upload is not None:
                self.after_upload(self, path.name)
            return
        if args[:2] == ['release', 'download']:
            name = args[args.index('--pattern') + 1]
            directory = Path(args[args.index('--dir') + 1])
            directory.mkdir(parents=True, exist_ok=True)
            (directory / name).write_bytes(self.asset_bytes[name])
            return
        raise AssertionError('Unexpected external/build command: ' + repr(args))

    def add_asset(self, name, content, *, state='uploaded', digest=True):
        self.next_asset += 1
        item = {'id': self.next_asset, 'name': name, 'state': state, 'size': len(content)}
        if digest:
            item['digest'] = 'sha256:' + hashlib.sha256(content).hexdigest()
        self.asset_records[name], self.asset_bytes[name] = item, content

    def commands(self, *prefix):
        return [event[1] for event in self.events
                if event[0] == 'command' and event[1][:len(prefix)] == prefix]


class ReleaseWorkflowTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='octomus-release-workflow-')
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        commands = patch.object(RELEASE.subprocess, 'run',
                                side_effect=AssertionError('Unexpected real external command'))
        commands.start()
        self.addCleanup(commands.stop)
        artifacts, metadata = {}, {}
        for image in RELEASE.IMAGES.IMAGES:
            OCIArchive(image).write(self.directory / (image + '.oci.tar'))
            item = RELEASE.IMAGES.metadata(self.directory, image, SOURCE, VERSION)
            metadata[image] = item
            artifacts['production-image-' + image] = {
                image + '.oci.tar': (self.directory / (image + '.oci.tar')).read_bytes(),
                image + '.json': (json.dumps(item) + '\n').encode(),
            }
        for platform in ('linux/amd64', 'linux/arm64'):
            name = 'production-acceptance-' + platform.split('/')[1]
            artifacts[name] = {name + '.json': (json.dumps(acceptance(platform, metadata)) + '\n').encode()}
        for name, target in zip(RELEASE.packages(TAG), RELEASE.TARGETS):
            artifacts['package-' + target] = {name: ('accepted package ' + target + '\n').encode()}
        self.peer = GitHubPeer(artifacts)
        self.attempt = 0

    def stage(self, mode='build', run_id=ORIGINAL_RUN, notes=''):
        self.attempt += 1
        return RELEASE.stage(self.peer, mode=mode, tag=TAG, source=SOURCE, version=VERSION,
                             notes=notes, run_id=run_id, directory=self.directory / ('attempt-' + str(self.attempt)))

    def interrupted(self, *, starter=False):
        self.peer.fail_upload_at = 3
        self.peer.leave_starter = starter
        with self.assertRaises(subprocess.CalledProcessError):
            self.stage()
        self.assertTrue(self.peer.release_record['draft'])
        self.assertEqual(self.peer.publications, [])
        return RELEASE.receipt_from(self.peer.release_record)

    def assert_published_matches(self, receipt):
        self.assertEqual(len(self.peer.publications), 1)
        release, assets, contents = self.peer.publications[0]
        self.assertFalse(release['draft'])
        self.assertEqual(RELEASE.receipt_from(release), receipt)
        self.assertEqual(set(assets), set(receipt['files']))
        for name, digest in receipt['files'].items():
            self.assertEqual(assets[name]['state'], 'uploaded')
            self.assertEqual(assets[name]['size'], len(contents[name]))
            self.assertEqual(hashlib.sha256(contents[name]).hexdigest(), digest)

    def test_new_release_pins_validated_inputs_in_draft_before_first_upload(self):
        self.assertEqual(RELEASE.prepare(self.peer, TAG, ''),
                         {'mode': 'build', 'source': SOURCE, 'version': VERSION})
        self.assertEqual(self.peer.commands('run', 'download'), [])
        receipt = self.stage()
        self.assertEqual(receipt['run_id'], ORIGINAL_RUN)
        self.assertEqual(receipt['source_commit'], SOURCE)
        created = next(index for index, event in enumerate(self.peer.events) if event[0] == 'created')
        uploads = [index for index, event in enumerate(self.peer.events)
                   if event[0] == 'command' and event[1][:2] == ('release', 'upload')]
        self.assertEqual(len(uploads), 6)
        self.assertLess(created, min(uploads))
        self.assertEqual(RELEASE.receipt_from(self.peer.events[created][1]), receipt)
        self.assertIn('Generated release notes.', self.peer.release_record['body'])
        self.assert_published_matches(receipt)

    def test_failed_native_acceptance_cannot_create_or_upload_release(self):
        name = 'production-acceptance-arm64'
        value = json.loads(self.peer.artifacts[ORIGINAL_RUN][name][name + '.json'])
        value['passed'] = False
        self.peer.artifacts[ORIGINAL_RUN][name][name + '.json'] = json.dumps(value).encode()
        with self.assertRaisesRegex(ValueError, 'acceptance did not pass'):
            self.stage()
        self.assertIsNone(self.peer.release_record)
        self.assertEqual(self.peer.commands('release', 'upload'), [])

    def test_partial_upload_retry_reuses_original_run_and_exact_hashes(self):
        receipt = self.interrupted()
        marker = RELEASE.receipt_marker(self.peer.release_record['body']).group(0)
        already_uploaded = set(self.peer.asset_records)
        # A new CI run exists, but its inputs must not substitute for the pinned original run.
        self.peer.artifacts[RETRY_RUN] = {'unexpected-rebuild': {'new-image': b'not accepted'}}
        self.assertEqual(RELEASE.prepare(self.peer, TAG, '')['mode'], 'retry')
        self.assertEqual(self.stage('retry', RETRY_RUN), receipt)
        self.assertEqual([args[2] for args in self.peer.commands('run', 'download')],
                         [str(ORIGINAL_RUN), str(ORIGINAL_RUN)])
        self.assertEqual(len(self.peer.commands('release', 'create')), 1)
        self.assertEqual(RELEASE.receipt_marker(self.peer.release_record['body']).group(0), marker)
        uploads = [Path(args[3]).name for args in self.peer.commands('release', 'upload')]
        for name in already_uploaded:
            self.assertEqual(uploads.count(name), 1)
        self.assert_published_matches(receipt)

    def test_complete_published_retry_uses_retained_assets_without_ci_download(self):
        receipt = self.stage()
        self.peer.artifacts.clear()  # Original workflow artifacts may already have expired.
        self.peer.events.clear()
        self.assertEqual(RELEASE.prepare(self.peer, TAG, '')['mode'], 'retry')
        self.assertEqual(self.stage('retry', RETRY_RUN), receipt)
        self.assertEqual(self.peer.commands('run', 'download'), [])
        self.assertEqual(self.peer.commands('release', 'upload'), [])
        self.assertEqual(self.peer.commands('release', 'create'), [])
        self.assertEqual(len(self.peer.commands('release', 'download')), 6)
        self.assert_published_matches(receipt)

    def test_promotion_download_uses_and_revalidates_published_assets(self):
        receipt = self.stage()
        self.peer.artifacts.clear()
        self.peer.events.clear()
        destination = self.directory / 'promotion'
        RELEASE.download(self.peer, TAG, SOURCE, VERSION, destination)
        RELEASE.validate_inputs(destination, receipt)
        self.assertEqual(len(self.peer.commands('release', 'download')), 6)
        self.assertEqual(self.peer.commands('run', 'download'), [])
        self.assertEqual(self.peer.commands('release', 'upload'), [])

    def test_published_release_with_missing_assets_cannot_fall_back_to_ci(self):
        self.interrupted()
        self.peer.release_record['draft'] = False
        self.peer.events.clear()
        with self.assertRaisesRegex(ValueError, 'Published release inputs are missing'):
            self.stage('retry', RETRY_RUN)
        self.assertEqual(self.peer.commands('run', 'download'), [])
        self.assertEqual(self.peer.commands('release', 'upload'), [])

    def test_notes_only_preserves_exact_opaque_receipt_without_source_reads(self):
        # A notes edit must preserve even an older/newer receipt schema it does not understand.
        marker = '<!-- octomus-release-inputs: {"schema":17, "future": true} -->'
        self.peer.release_record = {'id': 41, 'tag_name': TAG, 'draft': False,
                                   'body': 'Old notes.\n\n' + marker + '\n'}
        self.peer.source_available = False
        notes = 'Replacement notes.\n\nKeep intentional paragraphs.\n'
        self.assertEqual(RELEASE.prepare(self.peer, TAG, notes), {'mode': 'notes'})
        RELEASE.edit_notes(self.peer, TAG, notes)
        self.assertEqual(self.peer.release_record['body'], notes.rstrip() + '\n\n' + marker + '\n')
        self.assertEqual(self.peer.commands(), [])
        edits = [event[3] for event in self.peer.events if event[:2] == ('api', 'PATCH')]
        self.assertEqual(edits, [{'body': self.peer.release_record['body']}])

    def test_legacy_release_allows_notes_but_refuses_input_retry(self):
        self.peer.release_record = {'id': 41, 'tag_name': TAG, 'draft': False, 'body': 'Historical notes.'}
        self.peer.source_available = False
        with self.assertRaisesRegex(ValueError, 'no retained input receipt'):
            RELEASE.prepare(self.peer, TAG, '')
        self.assertEqual(RELEASE.prepare(self.peer, TAG, 'Updated legacy notes.'), {'mode': 'notes'})
        RELEASE.edit_notes(self.peer, TAG, 'Updated legacy notes.')
        self.assertEqual(self.peer.release_record['body'], 'Updated legacy notes.')
        self.assertEqual(self.peer.commands(), [])

    def test_notes_cannot_inject_or_silently_drop_ambiguous_receipt(self):
        marker = '<!-- octomus-release-inputs:{} -->'
        self.peer.release_record = {'id': 41, 'tag_name': TAG, 'draft': False,
                                   'body': marker + '\n' + marker}
        with self.assertRaisesRegex(ValueError, 'reserved input receipt'):
            RELEASE.edit_notes(self.peer, TAG, 'Injected ' + marker)
        with self.assertRaisesRegex(ValueError, 'Ambiguous release input receipt'):
            RELEASE.edit_notes(self.peer, TAG, 'Updated notes.')
        self.assertFalse(any(event[:2] == ('api', 'PATCH') for event in self.peer.events))

    def test_moved_tag_is_refused_before_staging_or_retry_download(self):
        self.assertEqual(RELEASE.prepare(self.peer, TAG, '')['mode'], 'build')
        self.peer.source = 'f' * 40
        with self.assertRaisesRegex(ValueError, 'source changed after preparation'):
            self.stage()
        self.assertEqual(self.peer.commands(), [])
        self.peer.source = SOURCE
        self.interrupted()
        self.peer.source = 'f' * 40
        self.peer.events.clear()
        with self.assertRaisesRegex(ValueError, 'tag moved away'):
            RELEASE.prepare(self.peer, TAG, '')
        with self.assertRaisesRegex(ValueError, 'source changed after preparation'):
            self.stage('retry', RETRY_RUN)
        self.assertEqual(self.peer.commands(), [])

    def test_changed_original_workflow_artifact_cannot_replace_pinned_inputs(self):
        receipt = self.interrupted()
        name = RELEASE.packages(TAG)[0]
        artifact = 'package-' + RELEASE.TARGETS[0]
        self.peer.artifacts[ORIGINAL_RUN][artifact][name] += b'changed original artifact'
        self.peer.events.clear()
        with self.assertRaisesRegex(ValueError, 'Original release asset bytes changed'):
            self.stage('retry', RETRY_RUN)
        self.assertEqual(self.peer.commands('release', 'upload'), [])
        self.assertEqual(RELEASE.receipt_from(self.peer.release_record), receipt)
        self.assertTrue(self.peer.release_record['draft'])

    def test_expired_original_artifacts_do_not_trigger_rebuild(self):
        self.interrupted()
        self.peer.artifacts.clear()
        self.peer.events.clear()
        with self.assertRaisesRegex(ValueError, 'Original accepted workflow artifacts are unavailable'):
            self.stage('retry', RETRY_RUN)
        self.assertEqual([args[2] for args in self.peer.commands('run', 'download')], [str(ORIGINAL_RUN)])
        self.assertEqual(self.peer.commands('release', 'create'), [])
        self.assertEqual(self.peer.commands('release', 'upload'), [])

    def test_changed_existing_asset_is_rejected_with_and_without_github_digest(self):
        self.interrupted()
        name = RELEASE.packages(TAG)[0]
        original = self.peer.asset_bytes[name]
        changed = bytes([original[0] ^ 1]) + original[1:]
        for have_digest in (True, False):
            with self.subTest(have_digest=have_digest):
                item = self.peer.asset_records[name]
                self.peer.asset_bytes[name] = changed
                if have_digest:
                    item['digest'] = 'sha256:' + hashlib.sha256(changed).hexdigest()
                else:
                    item.pop('digest', None)
                self.peer.events.clear()
                with self.assertRaisesRegex(ValueError, 'Existing release asset (digest|bytes) differ'):
                    self.stage('retry', RETRY_RUN)
                self.assertEqual(self.peer.commands('release', 'upload'), [])
                self.assertEqual(len(self.peer.commands('release', 'download')), 0 if have_digest else 1)
                self.assertTrue(self.peer.release_record['draft'])

    def test_missing_digest_is_verified_by_downloading_original_asset(self):
        receipt = self.interrupted()
        uploaded = set(self.peer.asset_records)
        for item in self.peer.asset_records.values():
            item.pop('digest', None)
        self.peer.events.clear()
        self.stage('retry', RETRY_RUN)
        downloaded = [args[args.index('--pattern') + 1] for args in self.peer.commands('release', 'download')]
        for name in uploaded:
            self.assertEqual(downloaded.count(name), 2)  # Before accepting it, then before publication.
        self.assert_published_matches(receipt)

    def test_final_remote_inventory_must_be_complete_and_match_before_publish(self):
        def corrupt_last_upload(peer, name):
            if len(peer.asset_records) == 6:
                peer.asset_records[RELEASE.packages(TAG)[0]]['digest'] = 'sha256:' + '0' * 64

        self.peer.after_upload = corrupt_last_upload
        with self.assertRaisesRegex(ValueError, 'Existing release asset digest differs'):
            self.stage()
        self.assertTrue(self.peer.release_record['draft'])
        self.assertEqual(self.peer.publications, [])
        self.assertEqual(len(self.peer.commands('release', 'upload')), 6)

    def test_disappeared_remote_asset_blocks_publication(self):
        def remove_after_last_upload(peer, name):
            if len(peer.asset_records) == 6:
                del peer.asset_records[RELEASE.packages(TAG)[0]]

        self.peer.after_upload = remove_after_last_upload
        with self.assertRaisesRegex(ValueError, 'Release asset disappeared during staging'):
            self.stage()
        self.assertTrue(self.peer.release_record['draft'])
        self.assertEqual(self.peer.publications, [])

    def test_starter_upload_cleanup_is_limited_to_failed_draft_upload(self):
        receipt = self.interrupted(starter=True)
        starter = next(item for item in self.peer.asset_records.values() if item['state'] == 'starter')
        self.peer.events.clear()
        self.stage('retry', RETRY_RUN)
        deletions = [event[2] for event in self.peer.events if event[:2] == ('api', 'DELETE')]
        self.assertEqual(deletions, ['releases/assets/' + str(starter['id'])])
        self.assert_published_matches(receipt)
        self.peer.events.clear()
        with self.assertRaisesRegex(ValueError, 'Only an incomplete draft upload'):
            self.peer.discard_starter(self.peer.release_record, starter)
        uploaded = next(iter(self.peer.asset_records.values()))
        with self.assertRaisesRegex(ValueError, 'Only an incomplete draft upload'):
            self.peer.discard_starter({**self.peer.release_record, 'draft': True}, uploaded)
        self.assertFalse(any(event[:2] == ('api', 'DELETE') for event in self.peer.events))


class GitHubDiscoveryTests(unittest.TestCase):
    def setUp(self):
        self.github = RELEASE.GitHub('fixture/releases')

    def test_missing_tag_uses_paginated_inventory_to_find_draft(self):
        draft = {'id': 41, 'tag_name': TAG, 'draft': True}
        first = [{'id': number, 'tag_name': 'other-' + str(number)} for number in range(100)]
        with patch.object(self.github, 'api', side_effect=[None, first, [draft]]) as api:
            self.assertEqual(self.github.release(TAG), draft)
        self.assertEqual([call.args[0] for call in api.call_args_list],
                         ['releases/tags/' + TAG, 'releases?per_page=100&page=1', 'releases?per_page=100&page=2'])
        self.assertTrue(api.call_args_list[0].kwargs['missing'])

    def test_published_tag_does_not_need_draft_inventory(self):
        release = {'id': 41, 'tag_name': TAG, 'draft': False}
        with patch.object(self.github, 'api', return_value=release) as api:
            self.assertEqual(self.github.release(TAG), release)
        api.assert_called_once_with('releases/tags/' + TAG, missing=True)

    def test_duplicate_drafts_and_duplicate_asset_names_are_refused(self):
        drafts = [{'id': identity, 'tag_name': TAG, 'draft': True} for identity in (41, 42)]
        with patch.object(self.github, 'api', side_effect=[None, drafts]):
            with self.assertRaisesRegex(ValueError, 'Multiple releases'):
                self.github.release(TAG)
        first = [{'id': number, 'name': 'asset-' + str(number)} for number in range(100)]
        with patch.object(self.github, 'api', side_effect=[first, [{'id': 200, 'name': 'asset-0'}]]):
            with self.assertRaisesRegex(ValueError, 'Duplicate release asset'):
                self.github.assets({'id': 41})

    def test_incomplete_or_malformed_paginated_inventory_is_refused(self):
        with patch.object(self.github, 'api', return_value=[{}] * 100) as api:
            with self.assertRaisesRegex(ValueError, 'pagination limit reached'):
                list(self.github.pages('releases'))
            self.assertEqual(api.call_count, 100)
        with patch.object(self.github, 'api', return_value={'message': 'not an inventory'}):
            with self.assertRaisesRegex(ValueError, 'Invalid paginated GitHub response'):
                list(self.github.pages('releases'))

    def test_only_explicit_http_404_is_classified_as_missing(self):
        cases = [
            ('gh: Not Found (HTTP 404)\n', True, True),
            ('gh: Not Found (HTTP 404)\r\n', True, True),
            ('gh: Not Found (HTTP 404)\n', False, False),
            ('gh: Bad credentials (HTTP 401)\n', True, False),
            ('gh: Resource not accessible by token (HTTP 403)\n', True, False),
            ('gh: error reading refs/tags/v404 (HTTP 401)\n', True, False),
            ('gh: failed request to https://example.test/404\n', True, False),
        ]
        for stderr, missing, absent in cases:
            with self.subTest(stderr=stderr, missing=missing):
                response = subprocess.CompletedProcess([], 1, '', stderr)
                with patch.object(RELEASE.subprocess, 'run', return_value=response) as run:
                    if absent:
                        self.assertIsNone(self.github.api('releases/tags/' + TAG, missing=missing))
                    else:
                        with self.assertRaisesRegex(RuntimeError, 'GitHub API failed'):
                            self.github.api('releases/tags/' + TAG, missing=missing)
                self.assertEqual(run.call_args.args[0][:4], ['gh', 'api', '--hostname', 'github.com'])


if __name__ == '__main__':
    unittest.main()
