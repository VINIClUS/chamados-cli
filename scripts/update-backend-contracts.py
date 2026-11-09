#!/usr/bin/env python3
"""Explicit, verified contract updates; --check needs no network or credentials."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tempfile
import urllib.request

REQUIRED = frozenset(('openapi.yaml', 'responses.json', 'operations-input.schema.json',
                      'operations-result.schema.json', 'executor-manifest.schema.json', 'backup-manifest.schema.json'))
MAX_FILE_BYTES = 32 * 1024 * 1024


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def validate_manifest(raw, project, sha, expected):
    if not re.fullmatch(r'[0-9a-f]{40}', sha) or not re.fullmatch(r'[0-9a-f]{64}', expected):
        raise ValueError('Full backend SHA and manifest SHA256 are required')
    if digest(raw) != expected:
        raise ValueError('Manifest checksum mismatch')
    data = json.loads(raw)
    if data.get('schema_version') != 1 or str(data.get('project_id')) != str(project) or data.get('backend_sha') != sha:
        raise ValueError('Manifest provenance mismatch')
    files = data.get('files', {})
    if set(files) != REQUIRED:
        raise ValueError('Manifest must contain exactly the required contract files')
    for name, meta in files.items():
        if not re.fullmatch(r'[0-9a-f]{64}', meta.get('sha256', '')) or type(meta.get('size')) is not int or not 0 < meta['size'] <= MAX_FILE_BYTES:
            raise ValueError('Invalid file metadata: ' + name)
    return data


def validate_files(directory, manifest):
    for name, meta in manifest['files'].items():
        raw = (directory / name).read_bytes()
        if len(raw) != meta['size'] or digest(raw) != meta['sha256']:
            raise ValueError('Contract checksum mismatch: ' + name)


def check(root):
    directory = root / 'contracts/backend'
    lock = json.loads((root / 'contracts/backend.lock.json').read_bytes())
    manifest = validate_manifest((directory / 'manifest.json').read_bytes(), lock['project_id'],
                                 lock['backend_sha'], lock['manifest_sha256'])
    if lock.get('files') != manifest['files']:
        raise ValueError('Lock checksums differ from manifest')
    expected_url = lock['registry_url'].rstrip('/') + '/projects/' + str(lock['project_id']) + '/packages/generic/backend-contracts/' + lock['backend_sha']
    if lock.get('package_url') != expected_url:
        raise ValueError('Package location differs from lock provenance')
    validate_files(directory, manifest)


def update(root, project, sha, expected, registry, package_dir=None, token_env='GITLAB_TOKEN'):
    if not str(project).isdigit():
        raise ValueError('Numeric project ID required')
    if not re.fullmatch(r'https://[^?#]+/api/v4', registry.rstrip('/')):
        raise ValueError('HTTPS GitLab API v4 registry required')
    base = registry.rstrip('/') + '/projects/' + str(project) + '/packages/generic/backend-contracts/' + sha

    def fetch(name):
        if package_dir is not None:
            path = package_dir / name
            if path.is_symlink():
                raise ValueError('Package symlinks are prohibited')
            raw = path.read_bytes()
        else:
            token = os.environ.get(token_env, '')
            if not token:
                raise ValueError('Set ' + token_env + ' to explicitly update private contracts')
            header = 'JOB-TOKEN' if token_env == 'CI_JOB_TOKEN' else 'PRIVATE-TOKEN'
            request = urllib.request.Request(base + '/' + name, headers={header: token})
            with urllib.request.urlopen(request, timeout=60) as response:
                raw = response.read(MAX_FILE_BYTES + 1)
        if len(raw) > MAX_FILE_BYTES:
            raise ValueError('Contract exceeds size limit')
        return raw

    raw = fetch('manifest.json')
    manifest = validate_manifest(raw, project, sha, expected)
    parent = root / 'contracts'
    parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.backend-update-', dir=parent) as temporary:
        staging = Path(temporary) / 'snapshot'
        staging.mkdir()
        (staging / 'manifest.json').write_bytes(raw)
        for name in sorted(REQUIRED):
            (staging / name).write_bytes(fetch(name))
        validate_files(staging, manifest)
        lock = {'schema_version': 1, 'project_id': str(project), 'backend_sha': sha,
                'registry_url': registry.rstrip('/'), 'package_url': base,
                'manifest_sha256': expected, 'files': manifest['files']}
        pending_lock = Path(temporary) / 'lock.json'
        pending_lock.write_text(json.dumps(lock, indent=2, sort_keys=True) + '\n')
        snapshot = parent / 'backend'
        lock_path = parent / 'backend.lock.json'
        backup = Path(temporary) / 'previous'
        old_lock = lock_path.read_bytes() if lock_path.exists() else None
        moved = False
        installed = False
        try:
            if snapshot.exists():
                os.replace(snapshot, backup)
                moved = True
            os.replace(staging, snapshot)
            installed = True
            os.replace(pending_lock, lock_path)
        except BaseException:
            if installed:
                shutil.rmtree(snapshot)
            if moved:
                os.replace(backup, snapshot)
            if old_lock is not None:
                lock_path.write_bytes(old_lock)
            elif lock_path.exists():
                lock_path.unlink()
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--check', action='store_true')
    parser.add_argument('--project-id', default='74155066')
    parser.add_argument('--backend-sha')
    parser.add_argument('--manifest-sha256')
    parser.add_argument('--registry-url', default='https://gitlab.com/api/v4')
    parser.add_argument('--package-dir', type=Path)
    parser.add_argument('--token-env', default='GITLAB_TOKEN')
    args = parser.parse_args()
    try:
        if args.check:
            check(args.root)
        else:
            if not args.backend_sha or not args.manifest_sha256:
                parser.error('--backend-sha and --manifest-sha256 are required for an update')
            update(args.root, args.project_id, args.backend_sha, args.manifest_sha256,
                   args.registry_url, args.package_dir, args.token_env)
        print('Backend contracts verified')
    except (OSError, ValueError, KeyError) as error:
        parser.exit(1, 'Contract verification failed: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
