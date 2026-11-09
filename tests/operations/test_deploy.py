import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class DeployContract(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.calls = self.path / "calls"
        self.bin = self.path / "bin"
        self.bin.mkdir()
        # These cases exercise manual invocation even when pytest runs inside CI.
        self.env = {
            **os.environ,
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "CALLS": str(self.calls),
            "CI_COMMIT_BRANCH": "",
            "CI_COMMIT_REF_PROTECTED": "false",
            "CI_JOB_TOKEN": "",
        }
        for name, body in {
            "ssh": '#!/bin/bash\nprintf "%s\\n" "$*" >>"$CALLS"\ncat >/dev/null\n',
            "curl": "#!/bin/bash\nprintf 200\n",
        }.items():
            f = self.bin / name
            f.write_text(body)
            f.chmod(0o755)

    def run_deploy(self, *args):
        return subprocess.run(
            ["bash", str(ROOT / "scripts/deploy.sh"), *args], env=self.env, capture_output=True, text=True
        )

    def test_staging_default_deploy_preserves_data(self):
        result = self.run_deploy("staging", "deploy", "--ref", "HEAD")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(" keep staging", self.calls.read_text())
        self.assertNotIn(" reset staging", self.calls.read_text())

    def test_reset_requires_matching_confirmation(self):
        for args in (
            ("staging", "reset"),
            ("staging", "reset", "--confirm", "production"),
            ("production", "reset", "--confirm", "production"),
        ):
            self.assertEqual(self.run_deploy(*args).returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_archive_uses_exact_revision_and_ref_under_remote_lock(self):
        archive = self.path / "target.tar"
        archive.write_bytes(b"source artifact")
        result = self.run_deploy(
            "staging", "deploy", "--archive", str(archive), "--revision", "b" * 40, "--source-ref", "feature/safe"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls.read_text()
        self.assertIn("flock -n -E 75 /opt/chamados/.deploy.lock", calls)
        self.assertIn("b" * 40, calls)
        self.assertIn("feature/safe", calls)
        self.assertIn("/opt/chamados/app/REF", calls)

    def test_archive_rejects_shell_revision_before_ssh(self):
        archive = self.path / "target.tar"
        archive.touch()
        result = self.run_deploy(
            "staging", "deploy", "--archive", str(archive), "--revision", "$(id)", "--source-ref", "main"
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_production_requires_release_tag_before_ssh(self):
        result = self.run_deploy("production", "deploy", "--ref", "HEAD")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_production_rejects_direct_deploy_even_with_semver_tag(self):
        archive = self.path / "target.tar"
        archive.touch()
        result = self.run_deploy(
            "production", "deploy", "--archive", str(archive), "--revision", "b" * 40, "--source-ref", "v9.9.9"
        )
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_production_cannot_run_authenticated_smoke(self):
        result = self.run_deploy("production", "deploy", "--ref", "v1.2.3", "--smoke")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())
