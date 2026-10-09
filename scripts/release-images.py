#!/usr/bin/env python3
"""Record, verify and promote the exact production OCI images accepted on both native release architectures."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile

IMAGES = ('octomus-agent', 'octomus-sandbox')
PLATFORMS = ('linux/amd64', 'linux/arm64')
CHECKS = {'non_root', 'no_capabilities', 'no_new_privileges', 'seccomp', 'read_only_image',
          'no_orchestrator_state', 'no_direct_egress', 'no_external_dns', 'no_host_route',
          'resource_limits', 'egress_gateway'}
SHA = re.compile(r'^sha256:[0-9a-f]{64}$')


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open('rb') as file:
        for chunk in iter(lambda: file.read(1 << 20), b''):
            digest.update(chunk)
    return digest.hexdigest()


def inspect_archive(path):
    """Read OCI descriptors without extracting archive paths or converting manifests."""
    with tarfile.open(path, 'r:*') as archive:
        members = {}
        for member in archive.getmembers():
            name = member.name.removeprefix('./').rstrip('/')
            if not name and member.isdir():
                name = '.'
            require(not name.startswith('/') and '..' not in PurePosixPath(name).parts and
                    name == str(PurePosixPath(name)) and (member.isfile() or member.isdir()), 'Unsafe OCI archive member')
            require(name not in members, 'Duplicate OCI archive member ' + name)
            members[name] = member

        def document(name, digest=None):
            member = members.get(name)
            require(member is not None and member.isfile() and member.size <= 96 << 20,
                    f'{path.name}: missing or oversized OCI document {name}')
            with archive.extractfile(member) as file:
                data = file.read()
            if digest:
                require('sha256:' + hashlib.sha256(data).hexdigest() == digest,
                        f'{path.name}: corrupt OCI document {name}')
            return json.loads(data)

        def blob(digest):
            require(isinstance(digest, str) and SHA.fullmatch(digest), 'Invalid OCI digest')
            return document('blobs/sha256/' + digest.removeprefix('sha256:'), digest)

        layout = document('index.json')
        require(len(layout.get('manifests', [])) == 1, 'OCI archive must name exactly one multi-platform image')
        digest = layout['manifests'][0]['digest']
        index = blob(digest)
        platforms, attestations, identities = {}, {}, []
        for descriptor in index.get('manifests', []):
            platform = descriptor.get('platform', {})
            name = platform.get('os', '') + '/' + platform.get('architecture', '')
            if name in PLATFORMS:
                require(name not in platforms, 'Duplicate OCI release platform ' + name)
                manifest = blob(descriptor['digest'])
                config = blob(manifest['config']['digest'])
                require(config['os'] + '/' + config['architecture'] == name, 'OCI config platform mismatch')
                labels = config.get('config', {}).get('Labels', {})
                identities.append({'source_commit': labels.get('org.opencontainers.image.revision'),
                                   'version': labels.get('org.opencontainers.image.version'),
                                   'image': labels.get('org.opencontainers.image.title')})
                platforms[name] = {'manifest_digest': descriptor['digest'], 'image_id': manifest['config']['digest']}
            elif descriptor.get('annotations', {}).get('vnd.docker.reference.type') == 'attestation-manifest':
                subject = descriptor['annotations'].get('vnd.docker.reference.digest')
                attestation = blob(descriptor['digest'])
                require(name == 'unknown/unknown' and isinstance(subject, str) and SHA.fullmatch(subject),
                        'Invalid attestation platform or subject')
                if 'subject' in attestation:
                    require(attestation['subject'].get('digest') == subject, 'Attestation manifest subject mismatch')
                predicates = attestations.setdefault(subject, set())
                for layer in attestation.get('layers', []):
                    require(layer.get('mediaType') == 'application/vnd.in-toto+json', 'Unknown attestation layer type')
                    statement = blob(layer['digest'])
                    predicate = statement.get('predicateType', '')
                    require(predicate == layer.get('annotations', {}).get('in-toto.io/predicate-type'),
                            'Attestation predicate annotation mismatch')
                    require(statement.get('_type') in ('https://in-toto.io/Statement/v0.1', 'https://in-toto.io/Statement/v1') and
                            any(item.get('digest', {}).get('sha256') == subject.removeprefix('sha256:')
                                for item in statement.get('subject', [])), 'Attestation does not describe its image manifest')
                    require(isinstance(statement.get('predicate'), dict) and statement['predicate'], 'Empty attestation predicate')
                    require(predicate not in predicates, 'Duplicate attestation predicate for an image')
                    predicates.add(predicate)
            else:
                raise ValueError('Refusing to promote an untested OCI descriptor: ' + name)
        require(set(platforms) == set(PLATFORMS), 'Both linux/amd64 and linux/arm64 must be present')
        for platform, image in platforms.items():
            predicates = attestations.get(image['manifest_digest'], set())
            require('https://spdx.dev/Document' in predicates and
                    any(value.startswith('https://slsa.dev/provenance/') for value in predicates),
                    'Missing SBOM or provenance for ' + platform)
        require(set(attestations) == {item['manifest_digest'] for item in platforms.values()},
                'Attestation refers to an image outside the tested platforms')
        require(all(identity == identities[0] for identity in identities), 'Image identity differs between platforms')
        return {**identities[0], 'digest': digest, 'platforms': platforms, 'sha256': sha256_file(path)}


def metadata(directory, image, source, version):
    require(image in IMAGES, 'Unknown production image')
    require(re.fullmatch(r'[0-9a-f]{40}', source), 'Source commit must be a full Git SHA')
    require(re.fullmatch(r'[0-9][a-zA-Z0-9.+-]*', version), 'Invalid release version')
    archive = image + '.oci.tar'
    inspected = inspect_archive(directory / archive)
    require(inspected['image'] == image and inspected['source_commit'] == source and inspected['version'] == version,
            'OCI image labels do not match the requested image, source commit and version')
    return {'schema': 1, 'image': image, 'source_commit': source, 'version': version, 'archive': archive,
            **inspected}


def check_receipt(receipt, inputs):
    platform = receipt.get('platform')
    require(platform in PLATFORMS and receipt.get('passed') is True, 'Native image acceptance did not pass')
    require(receipt.get('source_commit') == next(iter(inputs.values()))['source_commit'], 'Acceptance source commit mismatch')
    require(receipt.get('cleanup') == {'sandboxes': 0, 'leases': 0}, 'Sandbox or egress lease cleanup was not proven')
    for name, item in inputs.items():
        expected = {'digest': item['digest'], **item['platforms'][platform]}
        require(receipt.get('images', {}).get(name) == expected, 'Acceptance image identity mismatch: ' + name)
    docker_ids = receipt.get('docker_image_ids', {})
    require(set(docker_ids) == set(IMAGES), 'Both Docker image identities are required')
    for name, item in inputs.items():
        # Classic Docker stores identify an image by its config; containerd stores use its manifest. The harness
        # pulls the exact native manifest from this retained index, so either identity is bound to the tested bytes.
        native = item['platforms'][platform]
        require(docker_ids[name] in (native['image_id'], native['manifest_digest']),
                'Docker image identity is outside the accepted native manifest: ' + name)
    containers = receipt.get('runner_containers', {})
    require(set(containers) == {'codex', 'opencode'} and
            all(isinstance(ids, list) and len(ids) == 2 for ids in containers.values()),
            'Both clients must have inspected containers before and after resume')
    ids = [identity for group in containers.values() for identity in group]
    require(all(isinstance(identity, str) and re.fullmatch(r'[0-9a-f]{64}', identity) for identity in ids) and
            len(set(ids)) == 4, 'Runner resume must use distinct inspected containers')
    result = receipt.get('contract', {})
    require(result.get('passed') is True and result.get('broker_live') == 0, 'Production runner contract did not finish')
    records = result.get('contracts', {})
    require(set(records) == {'codex', 'opencode'}, 'Both production clients must pass')
    image_id = docker_ids['octomus-sandbox']
    for backend, runs in records.items():
        require(len(runs) == 2, backend + ' must complete and reconnect in distinct containers')
        for run in runs:
            require(run.get('image_id') == image_id and type(run.get('runs')) is int and run['runs'] == 1 and
                    run.get('incomplete') is False and run.get('oom') is False and
                    run.get('egress', {}).get('allowed', {}).get('provider.octomus.test:443', 0) > 0,
                    backend + ' lacks complete production-image provider CONNECT evidence')
    checks = result.get('containment', {}).get('checks', [])
    require(len(checks) == len(CHECKS) and {check.get('id') for check in checks} == CHECKS and
            all(check.get('passed') is True for check in checks), 'Containment checks did not pass')
    probe = result.get('containment_sandbox', {})
    require(probe.get('image_id') == image_id and type(probe.get('runs')) is int and probe['runs'] == 1 and
            probe.get('incomplete') is False and probe.get('oom') is False, 'Containment image evidence is incomplete')
    verification = result.get('verification', {})
    require(verification.get('image_id') == image_id and type(verification.get('runs')) is int and verification['runs'] == 1 and
            verification.get('incomplete') is False and verification.get('oom') is False,
            'Production-image verification was not proven')


def verify_inputs(directory, source, version, release=None):
    inputs = {}
    if release is not None:
        require(set(release.get('images', {})) == set(IMAGES), 'Release must contain exactly the two production images')
    for image in IMAGES:
        saved = (release['images'][image] if release is not None else
                 json.loads((directory / (image + '.json')).read_text()))
        actual = metadata(directory, image, source, version)
        require(saved == actual, 'Production image input changed: ' + image)
        inputs[image] = actual
    receipts = (release['acceptance'] if release is not None else
                [json.loads((directory / ('production-acceptance-' + platform.split('/')[1] + '.json')).read_text())
                 for platform in PLATFORMS])
    require(len(receipts) == 2 and {item.get('platform') for item in receipts} == set(PLATFORMS),
            'Both native architecture acceptance receipts are required')
    for receipt in receipts:
        check_receipt(receipt, inputs)
    return inputs, receipts


def manifest(directory, source, version, tag):
    require(tag == 'v' + version, 'Release tag must equal v plus VERSION')
    images, acceptance = verify_inputs(directory, source, version)
    return {'schema': 1, 'source_commit': source, 'version': version, 'tag': tag,
            'images': images, 'acceptance': acceptance}


def verify_release(directory, source, version, tag):
    release = json.loads((directory / 'release-images.json').read_text())
    require(release.get('schema') == 1 and release.get('source_commit') == source and
            release.get('version') == version and release.get('tag') == tag and tag == 'v' + version,
            'Retained image inputs do not belong to this release tag and source commit')
    verify_inputs(directory, source, version, release)
    return release


def promote(directory, release, repository):
    require(re.fullmatch(r'ghcr\.io/[a-z0-9][a-z0-9._-]*', repository), 'Expected a lower-case GHCR owner namespace')
    require(set(release.get('images', {})) == set(IMAGES), 'Refusing unverified extra release images')
    for image in IMAGES:
        item = release['images'][image]
        ref = repository + '/' + image + ':' + release['version']
        existing = subprocess.run(['skopeo', 'inspect', '--raw', 'docker://' + ref], capture_output=True)
        if existing.returncode == 0:
            require('sha256:' + hashlib.sha256(existing.stdout).hexdigest() == item['digest'],
                    'Refusing to replace a version tag containing a different image: ' + ref)
        else:
            error = existing.stderr.decode(errors='replace').lower()
            require(any(message in error for message in ('manifest unknown', 'name unknown')),
                    'Cannot check existing registry image before promotion: ' + error)
            subprocess.run(['skopeo', 'copy', '--all', '--preserve-digests',
                            'oci-archive:' + str(directory / item['archive']), 'docker://' + ref], check=True)
        observed = subprocess.run(['skopeo', 'inspect', '--raw', 'docker://' + ref], check=True, capture_output=True)
        require('sha256:' + hashlib.sha256(observed.stdout).hexdigest() == item['digest'], 'Promoted digest changed: ' + ref)
        subprocess.run(['cosign', 'sign', '--yes', repository + '/' + image + '@' + item['digest']], check=True)
        print('Promoted and signed ' + repository + '/' + image + '@' + item['digest'], flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['metadata', 'assemble', 'verify', 'promote'])
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--image', choices=IMAGES)
    parser.add_argument('--tag')
    parser.add_argument('--repository')
    args = parser.parse_args()
    if args.command == 'metadata':
        result = metadata(args.directory, args.image, args.source, args.version)
        output = args.directory / (args.image + '.json')
    elif args.command == 'assemble':
        result = manifest(args.directory, args.source, args.version, args.tag)
        output = args.directory / 'release-images.json'
    else:
        release = verify_release(args.directory, args.source, args.version, args.tag)
        if args.command == 'promote':
            promote(args.directory, release, args.repository)
        return
    output.write_text(json.dumps(result, indent=2) + '\n')
    print('Recorded ' + str(output), flush=True)


if __name__ == '__main__':
    main()
