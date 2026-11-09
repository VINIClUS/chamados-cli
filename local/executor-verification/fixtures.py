"""Temporary fixture ownership, GitLab HTTP, archives and real executor invocation."""

import hashlib
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from contextlib import contextmanager
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Literal

HERE = Path(__file__).resolve().parent
SHA = "a" * 40
NEW_SHA = "b" * 40
BACKUP = "20261107000000.1"
ROUND_DEADLINE: float | None = None


def command(*args, cwd=None, env=None, data=None, check=True, timeout=45):
    if ROUND_DEADLINE is not None:
        timeout = min(timeout, max(0.001, ROUND_DEADLINE - time.monotonic()))
    process = subprocess.Popen(
        ["rtk", "proxy", *map(str, args)],
        cwd=cwd,
        env=env,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        out, err = process.communicate(data, timeout=timeout)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.communicate()
        raise
    result = subprocess.CompletedProcess(args, process.returncode, out, err)
    if check and result.returncode:
        raise AssertionError(f"{args}: exit={result.returncode}\n{result.stderr.decode(errors='replace')}")
    return result


def write_json(path, value):
    Path(path).write_text(json.dumps(value, sort_keys=True))


def archive(files, gzip=False, prefix=""):
    out = io.BytesIO()
    mode: Literal["w", "w:gz"] = "w:gz" if gzip else "w"
    with tarfile.open(fileobj=out, mode=mode) as tar:
        for name, content in files.items():
            item = tarfile.TarInfo(prefix + name)
            item.mode = 0o644
            item.size = len(content)
            tar.addfile(item, io.BytesIO(content))
    return out.getvalue()


class GitLab:
    def __init__(self):
        self.files = {}
        self.events = []
        self.source = b""
        self.protected = True
        self.fail_manifest = False
        self.post_uncertain = False
        self.put_failure = False
        self.packages = []
        self.health_failure = False
        self.journal_path: Path | None = None
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                pass

            def do_GET(self):
                self.handle_request()

            do_PUT = do_POST = do_DELETE = do_GET

            def handle_request(self):
                path = self.path.split("?", 1)[0]
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                item = {"method": self.command, "path": path, "body": None, "at": time.time()}
                if owner.journal_path is not None and owner.journal_path.exists():
                    item["journal"] = json.loads(owner.journal_path.read_text())["result"]
                if self.command == "POST" or "deployments/" in path:
                    item["body"] = json.loads(body or b"{}")
                owner.events.append(item)
                status, value = 200, b"ok"
                if path.startswith("/api/v4/"):
                    if self.headers.get("JOB-TOKEN") != "local-only":
                        status, value = 403, b"{}"
                    elif "/repository/commits/" in path:
                        ref = path.rsplit("/", 1)[-1]
                        value = {"id": ref if len(ref) == 40 and all(c in "abcdef0123456789" for c in ref) else SHA}
                    elif "/repository/tags/" in path:
                        value = {"protected": owner.protected, "commit": {"id": NEW_SHA}}
                    elif path.endswith("/repository/archive.tar.gz"):
                        value = owner.source
                    elif path.endswith("/deployments"):
                        value = {} if owner.post_uncertain else {"id": 17}
                        status = 201
                    elif "/deployments/" in path:
                        status, value = (503 if owner.put_failure else 200), {}
                    elif "/packages/generic/" in path:
                        key = path.split("/packages/generic/", 1)[1]
                        if self.command == "PUT":
                            if owner.fail_manifest and key.endswith("manifest.json"):
                                status, value = 503, b"{}"
                            else:
                                owner.files[key] = body
                                status, value = 201, b"{}"
                        elif key in owner.files:
                            value = owner.files[key]
                        else:
                            status, value = 404, b"{}"
                    elif path.endswith("/packages"):
                        value = owner.packages
                    else:
                        value = {}
                elif owner.health_failure:
                    status = 503
                elif path.endswith("/auth/csrf"):
                    value = {"token": "local-csrf"}
                elif path.endswith("/auth/me"):
                    value = {"secretaries": [{"id": 1}]}
                elif path.endswith("/dashboard/calls"):
                    value = {"totalActive": 0, "urgent": 0, "recentCalls": []}
                elif path.endswith("/calls/filter"):
                    value = {"content": [], "page": {"number": 0, "totalElements": 0}}
                if not isinstance(value, bytes):
                    value = json.dumps(value).encode()
                self.send_response(status)
                self.send_header("Content-Length", str(len(value)))
                self.end_headers()
                self.wfile.write(value)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=lambda: self.server.serve_forever(poll_interval=0.01), daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def package(self, package_dir, backup_id=BACKUP, environment="staging"):
        for name in ["backup.tar.gz", "manifest.json"]:
            key = f"chamados-backup-{environment}/{backup_id}/{name}"
            self.files[key] = (package_dir / name).read_bytes()


class Fixture:
    def __init__(self, root, api, real=False):
        self.root, self.api = root, api
        self.project = "chamados_verify_" + root.name.replace("-", "_")
        self.host = root / "host"
        self.incoming = root / "incoming"
        self.bin = root / "bin"
        for path in [self.host / "app", self.host / "tooling/deploy", self.incoming / "tooling/deploy", self.bin]:
            path.mkdir(parents=True, exist_ok=True)
        self.env_content = b"POSTGRES_USER=fixture\nPOSTGRES_PASSWORD=fixture\nPOSTGRES_DB=fixture\nAPP_INITIALIZER_ENABLED=false\nLOCAL_KEEP=yes\n"
        (self.host / ".env").write_bytes(self.env_content)
        (self.host / "app/REVISION").write_text(SHA)
        (self.host / "app/REF").write_text("main")
        self.files = {
            "docker-compose.yml": b"services: {}\n",
            "Dockerfile": b"FROM scratch\n",
            "src/web/Dockerfile": b"FROM scratch\n",
            "marker": b"requested-application",
        }
        self.set_source()
        self.control = {
            "real": real,
            "docker": shutil.which("docker"),
            "project": self.project,
            "events": str(root / "events.jsonl"),
            "state": str(root / "state.json"),
            "fault": "",
        }
        write_json(root / "state.json", {"database": "original", "attachments": "original", "backend": True})
        self.save_control()
        # safeEnvironment intentionally drops arbitrary variables. Wrapper configuration is a private file.
        wrapper = f"#!{sys.executable}\nimport sys\nsys.path.insert(0, {str(HERE)!r})\nfrom dependencies import main\nmain({str(root / 'control.json')!r})\n"
        (self.bin / "docker").write_text(wrapper)
        (self.bin / "docker").chmod(0o700)
        if not real:
            (self.bin / "git").write_text(
                "#!/bin/sh\ncase \" $* \" in *' rev-parse '*) printf '%s\\n' '" + NEW_SHA + "';; esac\nexit 0\n"
            )
            (self.bin / "git").chmod(0o700)
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"])
        self.counter = 0

    def set_source(self):
        data = archive(self.files)
        (self.incoming / "source.tar").write_bytes(data)
        self.target = {
            "schema_version": 1,
            "revision": NEW_SHA,
            "ref": "main",
            "sha256": hashlib.sha256(data).hexdigest(),
        }
        self.api.source = archive(self.files, gzip=True, prefix="project/")

    def save_control(self, fault=None):
        if fault is not None:
            self.control["fault"] = fault
        write_json(self.root / "control.json", self.control)

    def envelope(self, operation, **overrides):
        self.counter += 1
        request = {
            "operation": operation,
            "environment": "staging",
            "ref": "main",
            "tail": 200,
            "confirm": "staging",
            "backup_id": BACKUP,
        }
        request.update(overrides)
        return {
            "protocol": 1,
            "operation_id": f"100-{self.counter}",
            "executor_sha": SHA,
            "deadline": (datetime.now(UTC) + timedelta(seconds=30)).isoformat(),
            "request": request,
            "target": dict(self.target),
            "settings": {
                "api_url": self.api.url + "/api/v4",
                "project_id": "42",
                "project_path": "local/fixture",
                "pipeline_iid": str(self.counter),
                "job_token": "local-only",
            },
        }

    def execute(self, envelope, health=None):
        # Install consumes the uploaded tooling directory. Each subsequent direct invocation
        # receives a fresh upload, just like the real SSH transport does.
        if not (self.incoming / "tooling").exists():
            shutil.copytree(self.host / "tooling", self.incoming / "tooling")
        journal = self.host / "operations" / (envelope["operation_id"] + ".json")
        self.control["journal"] = str(journal)
        self.api.journal_path = journal
        self.save_control()
        data = {
            "mode": "host",
            "directory": str(self.host),
            "incoming": str(self.incoming),
            "project": self.project,
            "health_url": health or self.api.url,
            "envelope": envelope,
        }
        output = command(HERE / "bin/probe", data=json.dumps(data).encode(), env=self.env)
        result = json.loads(output.stdout)
        if result.get("error"):
            result["diagnostics"] = output.stderr.decode(errors="replace")[-4000:]
        return result

    def events(self):
        path = self.root / "events.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def state(self):
        return json.loads((self.root / "state.json").read_text())

    def pack(self, raw, output, revision=NEW_SHA, environment="staging"):
        data = {
            "mode": "pack",
            "directory": str(raw),
            "output": str(output),
            "manifest": {
                "schema_version": 1,
                "environment": environment,
                "revision": revision,
                "ref": "main",
                "postgres_major": 16,
                "created_at": "2026-11-07T00:00:00Z",
            },
        }
        result = json.loads(command(HERE / "bin/probe", data=json.dumps(data).encode()).stdout)
        assert not result["error"], result

    def seed_package(self, environment="staging"):
        raw = self.root / "raw"
        raw.mkdir()
        (raw / "database.dump").write_bytes(b"PGDMPcontrolled-database")
        (raw / "attachments.tar").write_bytes(archive({"fixture.txt": b"snapshot"}))
        output = self.root / "package"
        self.pack(raw, output, environment=environment)
        self.api.package(output, environment=environment)
        return output


@contextmanager
def fixture(real=False):
    with tempfile.TemporaryDirectory(prefix="executor-check-") as directory:
        api = GitLab()
        try:
            yield Fixture(Path(directory), api, real)
        finally:
            api.close()
