"""Real HTTP transport contract against an isolated in-memory GitLab fixture."""

import http.server
import importlib.util
import json
import os
import subprocess
import threading

import pytest
from test_backup_archive import HELPER, snapshot


@pytest.fixture
def registry():
    store, requests = {}, []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            pass

        def do_PUT(self):
            assert self.headers.get("JOB-TOKEN") == "ephemeral-job-token"
            requests.append(("PUT", self.path))
            body = self.rfile.read(int(self.headers["Content-Length"]))
            if store.get("__reject_put__"):
                self.send_response(500)
                self.end_headers()
                return
            store[self.path] = body
            self.send_response(201)
            self.end_headers()

        def do_GET(self):
            requests.append(("GET", self.path))
            body = (
                json.dumps(store["__packages__"]).encode()
                if "__packages__" in store and "/packages?" in self.path
                else store.get(self.path)
            )
            self.send_response(200 if body is not None else 404)
            self.end_headers()
            if body is not None:
                self.wfile.write(body)

        def do_DELETE(self):
            requests.append(("DELETE", self.path))
            self.send_response(204)
            self.end_headers()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/api/v4", store, requests
    finally:
        server.shutdown()
        thread.join()


def command(registry, *args):
    env = {
        **os.environ,
        "CI_JOB_TOKEN": "ephemeral-job-token",
        "CI_API_V4_URL": registry[0],
        "CI_PROJECT_ID": "74155066",
        "OPS_ENVIRONMENT": "staging",
    }
    return subprocess.run(["python3", str(HELPER), *map(str, args)], env=env, capture_output=True, text=True)


def test_registry_publication_commits_manifest_last_and_refuses_overwrite(registry, tmp_path):
    output = snapshot(tmp_path)
    version = "20261107120000.42"
    result = command(registry, "publish", output, version)
    assert result.returncode == 0, result.stderr
    puts = [path for method, path in registry[2] if method == "PUT"]
    assert puts == [
        f"/api/v4/projects/74155066/packages/generic/chamados-backup-staging/{version}/backup.tar.gz",
        f"/api/v4/projects/74155066/packages/generic/chamados-backup-staging/{version}/manifest.json",
    ]
    assert command(registry, "publish", output, version).returncode != 0
    download = tmp_path / "download"
    result = command(registry, "download", version, download)
    assert result.returncode == 0, result.stderr
    assert (download / "backup.tar.gz").read_bytes() == (output / "backup.tar.gz").read_bytes()


def test_production_requires_matching_protected_tag_and_main_ancestry(registry, tmp_path, monkeypatch):
    _, store, _ = registry
    repository = tmp_path / "git-source"
    subprocess.run(["git", "init", "-q", "-b", "main", repository], check=True)

    def git(*args):
        return subprocess.run(
            ["git", "-C", str(repository), "-c", "user.name=Fixture", "-c", "user.email=test@example.invalid", *args],
            capture_output=True,
            text=True,
            check=True,
        ).stdout.strip()

    (repository / "file").write_text("snapshot")
    git("add", ".")
    git("commit", "-qm", "snapshot")
    sha = git("rev-parse", "HEAD")
    git("tag", "v1.2.3")
    (repository / "file").write_text("main later")
    git("commit", "-qam", "later")
    store["/api/v4/projects/74155066/repository/tags/v1.2.3"] = json.dumps(
        {"protected": True, "commit": {"id": sha}}
    ).encode()
    spec = importlib.util.spec_from_file_location("backup_production", HELPER)
    assert spec is not None and spec.loader is not None
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    for key, value in dict(
        CI_JOB_TOKEN="ephemeral-job-token", CI_API_V4_URL=registry[0], CI_PROJECT_ID="74155066"
    ).items():
        monkeypatch.setenv(key, value)
    monkeypatch.setenv("CI_PROJECT_PATH", "moved-group/MovedRepository")
    actual_run = subprocess.run
    remote_urls = []

    def local_transport(args, **kwargs):
        # The boundary double replaces only remote Git transport. Git still
        # fetches actual history and executes the real ancestry check.
        remote_urls.extend(arg for arg in args if arg.startswith("http://127.0.0.1:"))
        args = [str(repository) if arg.startswith("http://127.0.0.1:") else arg for arg in args]
        return actual_run(args, **kwargs)

    monkeypatch.setattr(helper.subprocess, "run", local_transport)
    helper.verify_production(sha, "v1.2.3")
    assert remote_urls == [registry[0].removesuffix("/api/v4") + "/moved-group/MovedRepository.git"]
    store["/api/v4/projects/74155066/repository/tags/v1.2.3"] = json.dumps(
        {"protected": False, "commit": {"id": sha}}
    ).encode()
    with pytest.raises(ValueError, match="unprotected"):
        helper.verify_production(sha, "v1.2.3")
    with pytest.raises(ValueError, match="immutable"):
        helper.verify_production(sha, "main")


def test_missing_archive_never_yields_a_completed_download(registry, tmp_path):
    version = "20261107120000.42"
    registry[1][f"/api/v4/projects/74155066/packages/generic/chamados-backup-staging/{version}/manifest.json"] = b"{}"
    assert command(registry, "download", version, tmp_path / "out").returncode != 0


def test_cleanup_keeps_latest_valid_and_only_deletes_exact_old_project_packages(registry, tmp_path):
    output = snapshot(tmp_path)
    versions = ["20260101000000.2", "20200101000000.3"]
    for version in versions:
        assert command(registry, "publish", output, version).returncode == 0

    def package(id, version, name="chamados-backup-staging", created="2020-01-01T00:00:00Z"):
        return dict(id=id, name=name, version=version, package_type="generic", created_at=created)

    registry[1]["__packages__"] = [
        package(1, "29990101000000.1", created="2999-01-01T00:00:00Z"),
        package(2, versions[0]),
        package(3, versions[1]),
        package(4, "20200101000000.4", "chamados-backup-staging-other"),
        package(5, "20200101000000.5"),
    ]
    result = command(registry, "cleanup")
    assert result.returncode == 0, result.stderr
    assert [path for method, path in registry[2] if method == "DELETE"] == [
        "/api/v4/projects/74155066/packages/3",
        "/api/v4/projects/74155066/packages/5",
    ]


def test_cleanup_without_valid_backup_refuses_any_deletion(registry):
    registry[1]["__packages__"] = [
        dict(
            id=3,
            name="chamados-backup-staging",
            package_type="generic",
            version="20200101000000.3",
            created_at="2020-01-01T00:00:00Z",
        )
    ]
    assert command(registry, "cleanup").returncode != 0
    assert not any(method == "DELETE" for method, path in registry[2])
