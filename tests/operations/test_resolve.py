import json
import os
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class ResolveTarget(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.source = self.path / "origin"
        self.source.mkdir()
        self.git("init", "-b", "main")
        self.git("config", "user.email", "tests@example.invalid")
        self.git("config", "user.name", "Tests")
        (self.source / "app.txt").write_text("main\n")
        self.git("add", ".")
        self.git("commit", "-m", "main")
        self.git("checkout", "-b", "feature")
        (self.source / "app.txt").write_text("requested revision\n")
        (self.source / "scripts").mkdir()
        (self.source / "scripts/deploy.sh").write_text("malicious executor\n")
        self.git("add", ".")
        self.git("commit", "-m", "feature")
        self.sha = self.git("rev-parse", "HEAD").strip()
        self.clone = self.path / "clone"
        subprocess.run(["git", "clone", "--quiet", str(self.source), str(self.clone)], check=True)

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.source), *args], check=True, capture_output=True, text=True).stdout

    def resolve(self, ref="feature", environment="staging", directory=None):
        env = {
            **os.environ,
            "OPS_REF": ref,
            "OPS_ENVIRONMENT": environment,
            "CI_COMMIT_BRANCH": "main",
            "CI_COMMIT_REF_PROTECTED": "true",
        }
        return subprocess.run(
            ["bash", str(ROOT / "scripts/operations/resolve.sh")],
            cwd=directory or self.clone,
            env=env,
            capture_output=True,
            text=True,
        )

    def test_resolves_once_and_packages_requested_sha_without_executor(self):
        result = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        meta = json.loads((self.clone / ".ops-target/revision.json").read_text())
        self.assertEqual(meta["revision"], self.sha)
        self.assertEqual(meta["ref"], "feature")
        with tarfile.open(self.clone / ".ops-target/source.tar") as archive:
            member = archive.extractfile("app.txt")
            assert member is not None
            self.assertEqual(member.read(), b"requested revision\n")
            self.assertNotIn("scripts/deploy.sh", archive.getnames())

    def test_option_like_ref_cannot_become_git_argument(self):
        result = self.resolve("--upload-pack=touch /tmp/unsafe")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.clone / ".ops-target/source.tar").exists())

    def test_subdirectory_resolution_excludes_devops_and_cicd(self):
        for name in ("devops/tooling.txt", ".gitlab/pipeline.yml"):
            path = self.source / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("protected tooling")
        self.git("add", ".")
        self.git("commit", "-qm", "operational files")
        directory = self.clone / "devops"
        directory.mkdir(exist_ok=True)
        result = self.resolve(directory=directory)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(directory / ".ops-target/source.tar") as archive:
            names = archive.getnames()
            self.assertIn("app.txt", names)
            self.assertFalse(any(name.startswith(("devops/", "cicd/")) for name in names))

    def test_production_rejects_branch_before_packaging(self):
        result = self.resolve("main", "production")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.clone / ".ops-target/source.tar").exists())


if __name__ == "__main__":
    unittest.main()
