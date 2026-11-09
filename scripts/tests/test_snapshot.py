import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error


SCRIPT = Path(__file__).parents[1] / 'update-backend-contracts.py'


class SnapshotTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.package = self.root / 'package'
        self.package.mkdir()
        self.client = self.root / 'client'
        (self.client / 'contracts' / 'backend').mkdir(parents=True)
        (self.client / 'contracts' / 'backend' / 'old').write_text('preserve')
        (self.client / 'contracts' / 'backend.lock.json').write_text('old lock')
        self.sha = 'a' * 40
        self.files = {}
        for name in ('openapi.yaml', 'responses.json', 'operations-input.schema.json',
                     'operations-result.schema.json', 'executor-manifest.schema.json', 'backup-manifest.schema.json'):
            content = b'{"fixture":true}\n'
            (self.package / name).write_bytes(content)
            self.files[name] = {'sha256': hashlib.sha256(content).hexdigest(), 'size': len(content)}
        self.write_manifest()

    def write_manifest(self):
        data = {'schema_version': 1, 'project_id': '74155066', 'backend_sha': self.sha, 'files': self.files}
        raw = json.dumps(data).encode()
        (self.package / 'manifest.json').write_bytes(raw)
        self.digest = hashlib.sha256(raw).hexdigest()

    def update(self):
        spec = importlib.util.spec_from_file_location('snapshot', SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        module.update(self.client, '74155066', self.sha, self.digest,
                      'https://gitlab.com/api/v4', self.package)
        return module

    def test_network_404_preserves_previous_snapshot(self):
        spec = importlib.util.spec_from_file_location('snapshot', SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with patch.dict('os.environ', {'GITLAB_TOKEN': 'synthetic-fixture-token'}):
            with patch.object(module.urllib.request, 'urlopen', side_effect=urllib.error.HTTPError(
                    'https://gitlab.com/fixture', 404, 'missing package', {}, None)):
                with self.assertRaises(urllib.error.HTTPError):
                    module.update(self.client, '74155066', self.sha, self.digest, 'https://gitlab.com/api/v4')
        self.assert_preserved()

    def assert_preserved(self):
        self.assertEqual('preserve', (self.client / 'contracts/backend/old').read_text())
        self.assertEqual('old lock', (self.client / 'contracts/backend.lock.json').read_text())

    def test_success_installs_snapshot_and_lock_and_checks_without_credentials(self):
        module = self.update()
        module.check(self.client)
        lock = json.loads((self.client / 'contracts/backend.lock.json').read_text())
        self.assertEqual(self.sha, lock['backend_sha'])
        self.assertEqual(self.digest, lock['manifest_sha256'])
        self.assertFalse((self.client / 'contracts/backend/old').exists())

    def test_nonexistent_package_preserves_previous_snapshot(self):
        self.package = self.root / 'missing'
        with self.assertRaises((ValueError, OSError)):
            self.update()
        self.assert_preserved()

    def test_invalid_checksum_preserves_previous_snapshot(self):
        (self.package / 'openapi.yaml').write_text('corrupt')
        with self.assertRaises(ValueError):
            self.update()
        self.assert_preserved()

    def test_missing_file_preserves_previous_snapshot(self):
        (self.package / 'responses.json').unlink()
        with self.assertRaises((ValueError, OSError)):
            self.update()
        self.assert_preserved()

    def test_wrong_backend_sha_preserves_previous_snapshot(self):
        self.sha = 'b' * 40
        with self.assertRaises(ValueError):
            self.update()
        self.assert_preserved()

    def test_manifest_path_traversal_preserves_previous_snapshot(self):
        self.files['../escape'] = self.files['responses.json']
        self.write_manifest()
        with self.assertRaises(ValueError):
            self.update()
        self.assert_preserved()


if __name__ == '__main__':
    unittest.main()
