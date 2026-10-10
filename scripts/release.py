#!/usr/bin/env python3
"""Stage verified release inputs once; retry uploads and promotion without rebuilding."""
import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.parse import quote


SPEC = importlib.util.spec_from_file_location('release_images', Path(__file__).with_name('release-images.py'))
IMAGES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(IMAGES)
TARGETS = ('x86_64-unknown-linux-gnu', 'aarch64-unknown-linux-gnu')
MARKER = 'octomus-release-inputs:'
MARKER_RE = re.compile(r'<!-- octomus-release-inputs:([^\r\n]*?) -->')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version_for(tag):
    # A version is also an OCI tag and a release asset filename. '+' is valid SemVer but not an OCI tag.
    require(isinstance(tag, str) and re.fullmatch(r'v[0-9][a-zA-Z0-9.-]{0,127}', tag),
            'Release tag must be v plus an OCI-compatible VERSION (digits, letters, dots and hyphens)')
    return tag[1:]


def packages(tag):
    return ['octomus-agent-' + tag + '-' + target + '.tar.gz' for target in TARGETS]


def asset_paths(directory, tag):
    return {**{name: directory / name for name in [*packages(tag), 'SHA256SUMS']},
            **{name: directory / 'images' / name for name in
               ['octomus-agent.oci.tar', 'octomus-sandbox.oci.tar', 'release-images.json']}}


def receipt_marker(body):
    body = body or ''
    matches = list(MARKER_RE.finditer(body))
    require(body.count(MARKER) == len(matches) and len(matches) <= 1, 'Ambiguous release input receipt')
    return matches[0] if matches else None


def receipt_from(release):
    marker = receipt_marker(release.get('body'))
    require(marker is not None, 'Release has no retained input receipt; recover its original tested inputs or use a new version')
    receipt = json.loads(marker.group(1))
    require(isinstance(receipt, dict) and set(receipt) ==
            {'schema', 'run_id', 'source_commit', 'version', 'tag', 'files'}, 'Invalid release input receipt')
    require(type(receipt['schema']) is int and receipt['schema'] == 1 and
            type(receipt['run_id']) is int and receipt['run_id'] > 0, 'Invalid original release workflow run')
    require(isinstance(receipt['source_commit'], str) and re.fullmatch(r'[0-9a-f]{40}', receipt['source_commit']),
            'Invalid release source commit')
    require(receipt['tag'] == release['tag_name'] and version_for(receipt['tag']) == receipt['version'],
            'Release input receipt belongs to another tag or version')
    require(isinstance(receipt['files'], dict) and set(receipt['files']) == set(asset_paths(Path(), receipt['tag'])) and
            all(isinstance(digest, str) and re.fullmatch(r'[0-9a-f]{64}', digest) for digest in receipt['files'].values()),
            'Release input receipt must pin exactly the six required assets')
    return receipt


def with_receipt(notes, receipt):
    require(MARKER not in notes, 'Release notes contain the reserved input receipt marker')
    return notes.rstrip() + '\n\n<!-- ' + MARKER + json.dumps(receipt, sort_keys=True, separators=(',', ':')) + ' -->\n'


class GitHub:
    def __init__(self, repository):
        require(re.fullmatch(r'[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+', repository), 'Invalid GitHub repository')
        self.repository = repository
        self.prefix = 'repos/' + repository + '/'

    def api(self, path, *, method='GET', data=None, missing=False):
        args = ['gh', 'api', '--hostname', 'github.com', '--method', method,
                '-H', 'Accept: application/vnd.github+json', self.prefix + path]
        if data is not None:
            args += ['--input', '-']
        result = subprocess.run(args, input=json.dumps(data) if data is not None else None,
                                capture_output=True, text=True)
        if result.returncode:
            if missing and re.search(r'\(HTTP 404\)(?:\r?\n|$)', result.stderr):
                return None
            raise RuntimeError('GitHub API failed: ' + result.stderr.strip())
        return json.loads(result.stdout) if result.stdout.strip() else None

    def pages(self, path):
        for page in range(1, 101):
            values = self.api(path + '?per_page=100&page=' + str(page))
            require(isinstance(values, list), 'Invalid paginated GitHub response')
            yield from values
            if len(values) < 100:
                return
        raise ValueError('GitHub pagination limit reached; refusing an incomplete release inventory')

    def release(self, tag):
        published = self.api('releases/tags/' + quote(tag, safe=''), missing=True)
        if published is not None:
            return published
        # Get-by-tag is documented for published releases. Drafts require a push-capable token and the list API.
        matches = [item for item in self.pages('releases') if item.get('tag_name') == tag]
        require(len(matches) <= 1, 'Multiple releases have the requested tag')
        return matches[0] if matches else None

    def assets(self, release):
        result = {}
        for item in self.pages('releases/' + str(release['id']) + '/assets'):
            require(item['name'] not in result, 'Duplicate release asset ' + item['name'])
            result[item['name']] = item
        return result

    def command(self, args):
        subprocess.run(['gh', *args, '--repo', self.repository], check=True)

    def download_artifacts(self, run_id, directory):
        try:
            self.command(['run', 'download', str(run_id), '--dir', str(directory), '--pattern', 'package-*',
                          '--pattern', 'production-image-*', '--pattern', 'production-acceptance-*'])
        except subprocess.CalledProcessError as error:
            raise ValueError('Original accepted workflow artifacts are unavailable; recover those inputs or use a new version') from error

    def download_asset(self, release, name, path):
        path.parent.mkdir(parents=True, exist_ok=True)
        self.command(['release', 'download', release['tag_name'], '--pattern', name, '--dir', str(path.parent)])

    def upload_asset(self, release, path):
        # Intentionally no --clobber. An uploaded asset is never replaced.
        self.command(['release', 'upload', release['tag_name'], str(path)])

    def create_draft(self, tag, notes, directory):
        path = directory / 'release-notes.md'
        path.write_text(notes)
        args = ['release', 'create', tag, '--draft', '--verify-tag', '--title', tag, '--notes-file', str(path)]
        if '-' in version_for(tag):
            args += ['--prerelease']
        self.command(args)

    def edit(self, release, **fields):
        return self.api('releases/' + str(release['id']), method='PATCH', data=fields)

    def discard_starter(self, release, asset):
        require(release['draft'] and asset['state'] == 'starter', 'Only an incomplete draft upload may be removed')
        self.api('releases/assets/' + str(asset['id']), method='DELETE')


def source_identity(github, tag):
    version = version_for(tag)
    ref = github.api('git/ref/tags/' + quote(tag, safe=''))
    obj = ref['object']
    for _ in range(8):
        require(isinstance(obj.get('sha'), str) and re.fullmatch(r'[0-9a-f]{40}', obj['sha']), 'Invalid release tag object')
        if obj['type'] == 'commit':
            break
        require(obj['type'] == 'tag', 'Release tag does not name a commit')
        obj = github.api('git/tags/' + obj['sha'])['object']
    else:
        raise ValueError('Release tag nesting limit exceeded')
    source = obj['sha']
    file = github.api('contents/VERSION?ref=' + source)
    require(file.get('type') == 'file' and file.get('encoding') == 'base64' and file.get('size', 4097) <= 4096,
            'Release source has no bounded VERSION file')
    text = base64.b64decode(file['content']).decode().strip()
    require(text == version, 'Release tag must equal v plus the VERSION file at its exact commit')
    return source, version


def prepare(github, tag, notes):
    require(isinstance(tag, str) and tag and '\x00' not in tag, 'Provide an existing release tag')
    require(MARKER not in notes, 'Release notes contain the reserved input receipt marker')
    release = github.release(tag)
    # Notes-only updates also work for historical releases without these scripts or the current version/schema.
    if release is not None and notes:
        return {'mode': 'notes'}
    receipt = receipt_from(release) if release is not None else None
    source, version = source_identity(github, tag)
    if receipt is not None:
        require(receipt['source_commit'] == source and receipt['version'] == version,
                'Release tag moved away from its originally accepted commit')
    return {'mode': 'retry' if receipt is not None else 'build', 'source': source, 'version': version}


def checksum_text(paths):
    return ''.join(IMAGES.sha256_file(path) + '  ' + path.name + '\n' for path in paths)


def artifact_inputs(github, run_id, source, version, tag, directory):
    raw = directory / 'artifacts'
    raw.mkdir(parents=True)
    github.download_artifacts(run_id, raw)
    inputs = directory / 'inputs'
    (inputs / 'images').mkdir(parents=True)
    sources = {name: 'package-' + target for name, target in zip(packages(tag), TARGETS)}
    sources.update({name: 'production-image-' + image for image in IMAGES.IMAGES
                    for name in (image + '.oci.tar', image + '.json')})
    sources.update({'production-acceptance-' + arch + '.json': 'production-acceptance-' + arch
                    for arch in ('amd64', 'arm64')})
    for name, artifact in sources.items():
        path = raw / artifact / name
        require(path.is_file() and not path.is_symlink() and not path.parent.is_symlink(),
                'Missing regular original workflow artifact ' + artifact + '/' + name)
        destination = inputs / name if name in packages(tag) else inputs / 'images' / name
        # Reuse files on the same workflow disk; large OCI archives need not be copied again.
        destination.hardlink_to(path)
    release_images = IMAGES.manifest(inputs / 'images', source, version, tag)
    (inputs / 'images' / 'release-images.json').write_text(json.dumps(release_images, indent=2) + '\n')
    (inputs / 'SHA256SUMS').write_text(checksum_text([inputs / name for name in packages(tag)]))
    return inputs


def make_receipt(directory, run_id, source, version, tag):
    return {'schema': 1, 'run_id': run_id, 'source_commit': source, 'version': version, 'tag': tag,
            'files': {name: IMAGES.sha256_file(path) for name, path in asset_paths(directory, tag).items()}}


def validate_inputs(directory, receipt):
    for name, path in asset_paths(directory, receipt['tag']).items():
        require(path.is_file() and not path.is_symlink() and IMAGES.sha256_file(path) == receipt['files'][name],
                'Original release asset bytes changed: ' + name)
    require((directory / 'SHA256SUMS').read_text() ==
            checksum_text([directory / name for name in packages(receipt['tag'])]), 'Release package checksums differ')
    IMAGES.verify_release(directory / 'images', receipt['source_commit'], receipt['version'], receipt['tag'])


def materialize_assets(github, release, receipt, directory):
    assets = github.assets(release)
    for name, path in asset_paths(directory, receipt['tag']).items():
        require(name in assets and assets[name]['state'] == 'uploaded', 'Required release asset is unavailable: ' + name)
        github.download_asset(release, name, path)
    validate_inputs(directory, receipt)


def verify_uploaded(github, release, asset, name, path, digest, directory):
    require(asset['state'] == 'uploaded' and asset['size'] == path.stat().st_size,
            'Existing release asset is incomplete or has a different size: ' + name)
    if asset.get('digest') is not None:
        require(asset['digest'] == 'sha256:' + digest, 'Existing release asset digest differs: ' + name)
        return
    # Older GitHub assets may omit digest. Download and compare before accepting or publishing them.
    with tempfile.TemporaryDirectory(prefix='verify-asset-', dir=directory) as scratch:
        downloaded = Path(scratch) / name
        github.download_asset(release, name, downloaded)
        require(IMAGES.sha256_file(downloaded) == digest, 'Existing release asset bytes differ: ' + name)


def stage(github, *, mode, tag, source, version, notes, run_id, directory):
    require(mode in ('build', 'retry'), 'Only a new release or an input-preserving retry may stage assets')
    require(source_identity(github, tag) == (source, version), 'Release source changed after preparation')
    directory.mkdir(parents=True, exist_ok=True)
    release = github.release(tag)
    if mode == 'build':
        require(release is None, 'Release appeared after preparation; retry the existing release without rebuilding')
        inputs = artifact_inputs(github, run_id, source, version, tag, directory)
        receipt = make_receipt(inputs, run_id, source, version, tag)
        if not notes:
            notes = github.api('releases/generate-notes', method='POST', data={'tag_name': tag})['body']
        # This atomic draft creation pins the original run AND every accepted byte before any asset upload.
        github.create_draft(tag, with_receipt(notes, receipt), directory)
        release = github.release(tag)
        require(release is not None and release['draft'] and receipt_from(release) == receipt,
                'Draft release does not contain the original accepted input receipt')
    else:
        require(release is not None, 'Original release disappeared; refusing to rebuild its images')
        receipt = receipt_from(release)
        require(receipt['source_commit'] == source and receipt['version'] == version, 'Original release identity differs')
        assets = github.assets(release)
        complete = all(name in assets and assets[name]['state'] == 'uploaded' for name in receipt['files'])
        if complete:
            inputs = directory / 'inputs'
            materialize_assets(github, release, receipt, inputs)
        else:
            require(release['draft'], 'Published release inputs are missing; recover the original assets before retrying')
            inputs = artifact_inputs(github, receipt['run_id'], source, version, tag, directory)
    validate_inputs(inputs, receipt)
    assets = github.assets(release)
    for name, path in asset_paths(inputs, tag).items():
        asset = assets.get(name)
        if asset is not None and asset['state'] == 'starter':
            github.discard_starter(release, asset)
            asset = None
        if asset is None:
            require(release['draft'], 'Refusing to add an unverified missing asset to a published release')
            github.upload_asset(release, path)
        else:
            verify_uploaded(github, release, asset, name, path, receipt['files'][name], directory)
    # Verify the complete remote set after uploads, then make it public. A failure retains the pinned draft.
    current = github.release(tag)
    require(current is not None and current['id'] == release['id'] and receipt_from(current) == receipt,
            'Release input receipt changed during staging')
    uploaded = github.assets(current)
    for name, path in asset_paths(inputs, tag).items():
        require(name in uploaded, 'Release asset disappeared during staging: ' + name)
        verify_uploaded(github, current, uploaded[name], name, path, receipt['files'][name], directory)
    require(source_identity(github, tag) == (source, version), 'Release tag moved while its assets were uploading')
    if current['draft']:
        github.edit(current, draft=False)
    return receipt


def download(github, tag, source, version, directory):
    require(source_identity(github, tag) == (source, version), 'Release tag moved after preparation')
    release = github.release(tag)
    require(release is not None and not release['draft'], 'Only published release inputs may be promoted')
    receipt = receipt_from(release)
    require(receipt['source_commit'] == source and receipt['version'] == version, 'Published release identity differs')
    materialize_assets(github, release, receipt, directory)


def edit_notes(github, tag, notes):
    require(notes and MARKER not in notes, 'Supply nonempty notes without the reserved input receipt marker')
    release = github.release(tag)
    require(release is not None, 'Release disappeared before its notes could be updated')
    marker = receipt_marker(release.get('body'))
    body = notes if marker is None else notes.rstrip() + '\n\n' + marker.group(0) + '\n'
    github.edit(release, body=body)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('prepare', 'stage', 'download', 'notes'))
    parser.add_argument('--directory', type=Path, default=Path('dist/release'))
    args = parser.parse_args()
    github = GitHub(os.environ['GH_REPO'])
    tag, notes = os.environ['RELEASE_TAG'], os.environ.get('RELEASE_NOTES', '')
    if args.command == 'prepare':
        result = prepare(github, tag, notes)
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.writelines(key + '=' + value + '\n' for key, value in result.items())
        print('Release operation: ' + result['mode'])
    elif args.command == 'notes':
        edit_notes(github, tag, notes)
    else:
        source, version = os.environ['RELEASE_SOURCE'], os.environ['RELEASE_VERSION']
        if args.command == 'stage':
            stage(github, mode=os.environ['RELEASE_MODE'], tag=tag, source=source, version=version, notes=notes,
                  run_id=int(os.environ['GITHUB_RUN_ID']), directory=args.directory)
        else:
            download(github, tag, source, version, args.directory)


if __name__ == '__main__':
    main()
