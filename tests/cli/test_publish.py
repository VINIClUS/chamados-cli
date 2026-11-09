"""Exercise the real release publisher against a local HTTP registry."""

import hashlib
import http.server
import json
import os
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dist = Path(self.tmp.name)
        self.files = {}
        self.release: dict[str, Any] | None = None
        self.writes = 0
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                pass

            def answer(self, status, payload=b""):
                self.send_response(status)
                self.end_headers()
                self.wfile.write(payload)

            def do_GET(self):
                if "/releases/" in self.path:
                    self.answer(404 if owner.release is None else 200, json.dumps(owner.release).encode())
                else:
                    self.answer(200 if self.path in owner.files else 404, owner.files.get(self.path, b""))

            def do_PUT(self):
                owner.files[self.path] = self.rfile.read(int(self.headers["Content-Length"]))
                owner.writes += 1
                self.answer(201)

            def do_POST(self):
                value = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path.endswith("/assets/links"):
                    assert owner.release is not None
                    owner.release["assets"]["links"].append(value)
                else:
                    owner.release = value
                owner.writes += 1
                self.answer(201, json.dumps(value).encode())

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        for target_os in ("linux", "darwin", "windows"):
            for arch in ("amd64", "arm64"):
                name = f"chamadosctl_0.1.0_{target_os}_{arch}"
                if target_os == "windows":
                    name += ".exe"
                (self.dist / name).write_bytes(f"fixture {target_os} {arch}".encode())
        self.checksums()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.tmp.cleanup()

    def checksums(self):
        (self.dist / "SHA256SUMS").write_text(
            "".join(
                f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n"
                for p in sorted(self.dist.glob("chamadosctl_*"))
            )
        )

    def publish(self):
        return subprocess.run(
            ["bash", str(ROOT / "scripts/cli/publish.sh"), str(self.dist)],
            env={
                **os.environ,
                "CI_API_V4_URL": f"http://127.0.0.1:{self.server.server_port}/api/v4",
                "CI_PROJECT_ID": "123",
                "CI_COMMIT_TAG": "cli-v0.1.0",
                "CI_JOB_TOKEN": "fixture-secret",
            },
            text=True,
            capture_output=True,
        )

    def test_publishes_six_binaries_and_checksums_and_can_retry(self):
        first = self.publish()
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(len(self.files), 7)
        assert self.release is not None
        self.assertEqual(len(self.release["assets"]["links"]), 7)
        writes = self.writes
        second = self.publish()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(self.writes, writes)
        self.assertNotIn("fixture-secret", first.stdout + first.stderr)

    def test_refuses_replacing_already_published_bytes(self):
        self.assertEqual(self.publish().returncode, 0)
        original = dict(self.files)
        (self.dist / "chamadosctl_0.1.0_linux_amd64").write_bytes(b"changed")
        self.checksums()
        second = self.publish()
        self.assertNotEqual(second.returncode, 0)
        self.assertEqual(self.files, original)

    def test_completes_asset_links_on_release_created_before_publisher(self):
        self.release = {"tag_name": "cli-v0.1.0", "description": "Notas do operador", "assets": {"links": []}}
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.release["assets"]["links"]), 7)
        self.assertEqual(self.release["description"], "Notas do operador")


if __name__ == "__main__":
    unittest.main()
