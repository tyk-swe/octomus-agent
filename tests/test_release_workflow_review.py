"""Independent regressions for release staging and historical notes-only operations."""
import base64
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_release_images import OCIArchive, SOURCE, TAG, VERSION, acceptance


PROJECT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('release_workflow_review', PROJECT / 'scripts/release.py')
RELEASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RELEASE)


class MovingTagGitHub:
    """A collaborator moves the Git tag while all originally accepted uploads succeed."""
    def __init__(self):
        self.source = SOURCE
        self.saved = None
        self.uploads = {}
        self.edits = []

    def api(self, path, **kwargs):
        if path == 'git/ref/tags/' + TAG:
            return {'object': {'type': 'commit', 'sha': self.source}}
        if path == 'contents/VERSION?ref=' + self.source:
            data = (VERSION + '\n').encode()
            return {'type': 'file', 'encoding': 'base64', 'size': len(data),
                    'content': base64.b64encode(data).decode()}
        raise AssertionError('Unexpected GitHub API call: ' + path)

    def release(self, tag):
        if tag != TAG:
            raise AssertionError('Unexpected tag')
        return copy.deepcopy(self.saved)

    def create_draft(self, tag, notes, directory):
        if self.saved is not None:
            raise AssertionError('Duplicate draft creation')
        self.saved = {'id': 1, 'tag_name': tag, 'draft': True, 'body': notes}

    def assets(self, release):
        return copy.deepcopy(self.uploads)

    def upload_asset(self, release, path):
        data = path.read_bytes()
        self.uploads[path.name] = {'id': len(self.uploads) + 1, 'name': path.name,
                                  'state': 'uploaded', 'size': len(data),
                                  'digest': 'sha256:' + hashlib.sha256(data).hexdigest()}
        self.source = 'f' * 40

    def edit(self, release, **fields):
        self.edits.append(fields)
        self.saved.update(fields)
        return copy.deepcopy(self.saved)


class ReleaseWorkflowReviewTests(unittest.TestCase):
    def test_historical_notes_only_update_does_not_require_an_OCI_compatible_tag(self):
        class HistoricalGitHub:
            def __init__(self):
                self.saved = {'id': 7, 'tag_name': 'v0.1.0+build.1', 'body': 'Historical release notes', 'draft': False}
                self.edits = []

            def release(self, tag):
                if tag != self.saved['tag_name']:
                    raise AssertionError('Wrong historical release')
                return copy.deepcopy(self.saved)

            def api(self, *args, **kwargs):
                raise AssertionError('A notes-only update must not read or validate a historical source checkout')

            def edit(self, release, **fields):
                self.edits.append(fields)
                self.saved.update(fields)

        github = HistoricalGitHub()
        operation = RELEASE.prepare(github, github.saved['tag_name'], 'Corrected release notes')
        self.assertEqual(operation, {'mode': 'notes'})
        RELEASE.edit_notes(github, github.saved['tag_name'], 'Corrected release notes')
        self.assertEqual(github.edits, [{'body': 'Corrected release notes'}])
        self.assertFalse(github.saved['draft'])

    def test_tag_moved_during_upload_keeps_all_accepted_assets_in_the_original_draft(self):
        with tempfile.TemporaryDirectory(prefix='octomus-staging-review-') as temporary:
            directory = Path(temporary)
            inputs = directory / 'accepted'
            images = inputs / 'images'
            images.mkdir(parents=True)
            metadata = {}
            for image in ('octomus-agent', 'octomus-sandbox'):
                OCIArchive(image).write(images / (image + '.oci.tar'))
                metadata[image] = RELEASE.IMAGES.metadata(images, image, SOURCE, VERSION)
                (images / (image + '.json')).write_text(json.dumps(metadata[image]))
            for platform in ('linux/amd64', 'linux/arm64'):
                (images / ('production-acceptance-' + platform.split('/')[1] + '.json')).write_text(
                    json.dumps(acceptance(platform, metadata)))
            image_receipt = RELEASE.IMAGES.manifest(images, SOURCE, VERSION, TAG)
            (images / 'release-images.json').write_text(json.dumps(image_receipt, indent=2) + '\n')
            # Package contents have already passed CI; this test exercises preservation and publication, not extraction.
            for name in RELEASE.packages(TAG):
                (inputs / name).write_bytes(('accepted package ' + name).encode())
            (inputs / 'SHA256SUMS').write_text(RELEASE.checksum_text([inputs / name for name in RELEASE.packages(TAG)]))
            github = MovingTagGitHub()
            with patch.object(RELEASE, 'artifact_inputs', return_value=inputs):
                with self.assertRaisesRegex(ValueError, '[Ss]ource|[Tt]ag'):
                    RELEASE.stage(github, mode='build', tag=TAG, source=SOURCE, version=VERSION,
                                  notes='Reviewed release', run_id=77, directory=directory / 'work')
            self.assertIsNotNone(github.saved)
            self.assertIs(github.saved['draft'], True)
            self.assertEqual(github.edits, [])
            self.assertEqual(set(github.uploads), set(RELEASE.asset_paths(inputs, TAG)))
            receipt = RELEASE.receipt_from(github.saved)
            self.assertEqual(receipt['source_commit'], SOURCE)
            self.assertEqual(receipt['run_id'], 77)
            RELEASE.validate_inputs(inputs, receipt)


if __name__ == '__main__':
    unittest.main()
