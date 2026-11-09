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

try:
    import yaml
except ImportError:  # PyYAML is installed by CI before contract updates.
    yaml = None

REQUIRED = frozenset(('openapi.yaml', 'responses.json', 'operations-input.schema.json',
                      'operations-result.schema.json', 'executor-manifest.schema.json', 'backup-manifest.schema.json'))
MAX_FILE_BYTES = 32 * 1024 * 1024


def _pointer(document, reference):
    if not isinstance(reference, str) or not reference.startswith("#/"):
        raise ValueError("OpenAPI references must stay inside the snapshot")
    current = document
    for part in reference[2:].split("/"):
        part = part.replace("~1", "/").replace("~0", "~")
        if not isinstance(current, dict) or part not in current:
            raise ValueError("OpenAPI reference points to a missing object: " + reference)
        current = current[part]
    return current


def validate_openapi(raw):
    if yaml is None:
        raise ValueError("PyYAML is required for OpenAPI contract validation")
    try:
        document = yaml.safe_load(raw)
    except yaml.YAMLError as error:
        raise ValueError("Invalid OpenAPI YAML: " + str(error)) from error
    if not isinstance(document, dict) or not isinstance(document.get("openapi"), str):
        raise ValueError("OpenAPI document missing openapi version")
    if not isinstance(document.get("info"), dict) or not document["info"].get("title"):
        raise ValueError("OpenAPI document missing info.title")
    paths = document.get("paths")
    if not isinstance(paths, dict) or not paths:
        raise ValueError("OpenAPI document must contain paths")
    components = document.get("components")
    if not isinstance(components, dict):
        raise ValueError("OpenAPI document must contain components")
    for path, item in paths.items():
        if not isinstance(path, str) or not path.startswith("/") or not isinstance(item, dict):
            raise ValueError("Invalid OpenAPI path item")
        for method, operation in item.items():
            if method not in {"get", "put", "post", "delete", "options", "head", "patch", "trace"}:
                continue
            if not isinstance(operation, dict):
                raise ValueError("Invalid OpenAPI operation: " + path)
            responses = operation.get("responses")
            if not isinstance(responses, dict) or not responses:
                raise ValueError("OpenAPI operation missing responses: " + path)
            for code, response in responses.items():
                if code != "default" and (not isinstance(code, str) or not code.isdigit() or len(code) != 3):
                    raise ValueError("Invalid OpenAPI response code: " + str(code))
                if not isinstance(response, dict) or ("$ref" not in response and "description" not in response):
                    raise ValueError("OpenAPI response missing description: " + path)
    refs = []

    def collect(value):
        if isinstance(value, dict):
            if "$ref" in value:
                refs.append(value["$ref"])
            for nested in value.values():
                collect(nested)
        elif isinstance(value, list):
            for nested in value:
                collect(nested)

    collect(document)
    for reference in refs:
        _pointer(document, reference)


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
    validate_openapi((directory / 'openapi.yaml').read_bytes())
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
        validate_openapi((staging / 'openapi.yaml').read_bytes())
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
