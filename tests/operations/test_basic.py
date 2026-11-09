import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class BasicOperations(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.bin = self.path / "bin"
        self.bin.mkdir()
        self.deploy = self.path / "deploy"
        (self.deploy / "app").mkdir(parents=True)
        (self.deploy / "app/REVISION").write_text("a" * 40 + "\n")
        self.calls = self.path / "calls"
        self.env = {
            **os.environ,
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "DEPLOY_DIR": str(self.deploy),
            "CALLS": str(self.calls),
        }
        self.executable("docker", '#!/bin/bash\nprintf "%s\\n" "$*" >>"$CALLS"\nprintf "container ready\\n"\n')
        self.executable("curl", '#!/bin/bash\nprintf "%s\\n" "$*" >>"$CALLS"\n')

    def executable(self, name, text):
        target = self.bin / name
        target.write_text(text)
        target.chmod(0o755)

    def remote(self, operation, environment="staging", service="", tail="200", confirm=""):
        return subprocess.run(
            ["bash", str(ROOT / "scripts/operations/remote-basic.sh"), environment, operation, service, tail, confirm],
            env=self.env,
            capture_output=True,
            text=True,
        )

    def test_logs_are_finite_and_service_is_one_argument(self):
        result = self.remote("logs", service="chamados-backend", tail="25")
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["revision"], "a" * 40)
        self.assertEqual(output["data"]["output"], "container ready\n")
        self.assertIn("logs --no-color --tail 25 chamados-backend", self.calls.read_text())
        self.assertNotIn(" -f ", self.calls.read_text().split(".yml", 2)[-1])

    def test_rejects_injection_before_docker(self):
        result = self.remote("logs", service="$(touch /tmp/unwanted)")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_rejects_unbounded_tail(self):
        for tail in ("0", "-1", "all", "10001", "1;id"):
            with self.subTest(tail=tail):
                self.assertEqual(self.remote("logs", tail=tail).returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_production_reset_and_smoke_never_reach_docker(self):
        for op in ("reset", "smoke"):
            self.assertEqual(self.remote(op, "production", confirm="production").returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_lock_blocks_even_read_operations(self):
        lock = subprocess.Popen(["flock", str(self.deploy / ".deploy.lock"), "sleep", "10"])
        self.addCleanup(lock.wait)
        self.addCleanup(lock.terminate)
        # Wait for the actual lock, without relying on a fixed scheduling delay.
        import fcntl
        import time

        with (self.deploy / ".deploy.lock").open("a") as handle:
            for _ in range(100):
                try:
                    fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    fcntl.flock(handle, fcntl.LOCK_UN)
                    time.sleep(0.01)
                except BlockingIOError:
                    break
            else:
                self.fail("test lock was not acquired")
        self.assertEqual(self.remote("status").returncode, 75)
        self.assertFalse(self.calls.exists())

    def test_remote_failure_cannot_report_success(self):
        self.executable("docker", "#!/bin/bash\nexit 9\n")
        result = self.remote("restart")
        self.assertEqual(result.returncode, 9)
        self.assertEqual(result.stdout, "")

    def test_health_checks_backend_and_frontend(self):
        result = self.remote("health")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("http://127.0.0.1:8181/api/auth/csrf", self.calls.read_text())
        self.assertIn("http://127.0.0.1/", self.calls.read_text())

    def test_restart_without_service_does_not_restart_database(self):
        result = self.remote("restart")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("restart chamados-backend chamados-frontend", self.calls.read_text())

    def test_dispatcher_rejects_unprotected_or_non_main_before_ssh(self):
        self.executable("ssh", '#!/bin/bash\ntouch "$CALLS"\n')
        for branch, protected in (("feature", "true"), ("main", "false")):
            env = {
                **self.env,
                "CI_COMMIT_BRANCH": branch,
                "CI_COMMIT_REF_PROTECTED": protected,
                "OPS_OPERATION": "status",
                "OPS_ENVIRONMENT": "staging",
            }
            result = subprocess.run(
                ["bash", str(ROOT / "scripts/operations/run.sh")],
                cwd=self.path,
                env=env,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads((self.path / "operation-result.json").read_text())["status"], "failed")
        self.assertFalse(self.calls.exists())


if __name__ == "__main__":
    unittest.main()
