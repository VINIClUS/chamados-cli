"""Disposable real Docker Compose + PostgreSQL, never /opt/chamados or remote CTs."""

import json
import os
import shutil
import subprocess
import tarfile
import uuid
from pathlib import Path

import pytest
from test_backup_registry import registry as registry

ROOT = Path(__file__).resolve().parents[1]
REMOTE = ROOT / "scripts/operations/remote-backup.sh"
SHA = "a" * 40


def shell(*args, **kwargs):
    return subprocess.run(list(map(str, args)), capture_output=True, text=True, **kwargs)


@pytest.fixture
def stack(tmp_path, registry):
    if shutil.which("docker") is None or shell("docker", "info").returncode:
        pytest.skip("Docker Engine required for disposable PostgreSQL integration")
    project = "backup-test-" + uuid.uuid4().hex[:10]
    deployed = tmp_path / "host"
    app = deployed / "app"
    (app / "api/auth").mkdir(parents=True)
    (app / "api/auth/csrf").write_text("ok")
    (app / "Dockerfile").write_text(
        'FROM python:3-alpine\nWORKDIR /srv\nCOPY . .\nCMD ["python","-m","http.server","8181"]\n'
    )
    (app / "src/web").mkdir(parents=True)
    (app / "src/web/Dockerfile").write_text((app / "Dockerfile").read_text())
    compose = """services:
  chamados-postgresql:
    image: postgres:16-alpine
    cpus: 0.5
    cpu_shares: 64
    mem_limit: 256m
    environment:
      POSTGRES_USER: chamados
      POSTGRES_PASSWORD: fixture-only
      POSTGRES_DB: chamados
    healthcheck:
      test: [CMD-SHELL, 'pg_isready -U chamados']
      interval: 1s
      retries: 30
    volumes: [postgres_data:/var/lib/postgresql/data]
  chamados-backend:
    build: .
    cpus: 0.25
    cpu_shares: 64
    mem_limit: 128m
    ports: ['127.0.0.1:18181:8181']
    volumes: [call_attachments:/app/data/call-attachments]
  chamados-frontend:
    build: .
    cpus: 0.25
    cpu_shares: 64
    mem_limit: 128m
    ports: ['127.0.0.1:18080:8181']
volumes:
  postgres_data:
  call_attachments:
"""
    (app / "docker-compose.yml").write_text(compose)
    (app / "REVISION").write_text(SHA + "\n")
    (app / "REF").write_text("main\n")
    (deployed / ".env").write_text("PRESERVED_SECRET=fixture-never-archive\n")
    tooling = deployed / "tooling/deploy"
    tooling.mkdir(parents=True)
    (tooling / "docker-compose.staging.yml").write_text("services: {}\n")
    compose_args = [
        "docker",
        "compose",
        "-p",
        project,
        "--env-file",
        str(deployed / ".env"),
        "-f",
        str(app / "docker-compose.yml"),
        "-f",
        str(tooling / "docker-compose.staging.yml"),
    ]
    result = shell(*compose_args, "up", "-d", "--build", "--wait", "--wait-timeout", "90")
    assert result.returncode == 0, result.stderr
    assert (
        shell(
            *compose_args,
            "exec",
            "-T",
            "chamados-postgresql",
            "psql",
            "-U",
            "chamados",
            "-d",
            "chamados",
            "-c",
            "CREATE TABLE fixture(value text); INSERT INTO fixture VALUES ('snapshot');",
        ).returncode
        == 0
    )
    assert (
        shell(
            *compose_args,
            "exec",
            "-T",
            "chamados-backend",
            "sh",
            "-c",
            "mkdir -p /app/data/call-attachments/nested; printf attachment-snapshot >/app/data/call-attachments/nested/test.txt",
        ).returncode
        == 0
    )
    registry[1][f"/api/v4/projects/74155066/repository/commits/{SHA}"] = json.dumps({"id": SHA}).encode()
    source = tmp_path / "source.tar.gz"
    with tarfile.open(source, "w:gz") as archive:
        archive.add(app, arcname="project-sha")
    registry[1][f"/api/v4/projects/74155066/repository/archive.tar.gz?sha={SHA}"] = source.read_bytes()
    config = dict(
        CI_JOB_TOKEN="ephemeral-job-token",
        CI_API_V4_URL=registry[0],
        CI_PROJECT_ID="74155066",
        CI_PIPELINE_IID="99",
        OPS_REF="main",
        OPS_CONFIRM="staging",
    )
    env = {
        **os.environ,
        "CHAMADOS_DEPLOY_DIR": str(deployed),
        "BACKUP_COMPOSE_PROJECT": project,
        "BACKUP_BACKEND_HEALTH_URL": "http://127.0.0.1:18181/api/auth/csrf",
        "BACKUP_FRONTEND_HEALTH_URL": "http://127.0.0.1:18080/",
    }

    def run(action, backup_id="", **settings):
        return shell("bash", REMOTE, action, "staging", backup_id, env={**env, **settings}, input=json.dumps(config))

    try:
        yield run, compose_args, deployed, registry
    finally:
        shell(*compose_args, "down", "-v", "--remove-orphans", "--rmi", "local")


def test_real_postgres_and_attachments_roundtrip_and_corruption_is_nonmutating(stack):
    run, compose, deployed, registry = stack
    env_before = (deployed / ".env").read_bytes()
    result = run("create")
    assert result.returncode == 0, result.stderr
    backup_id = json.loads(result.stdout)["backup_id"]
    shell(
        *compose,
        "exec",
        "-T",
        "chamados-postgresql",
        "psql",
        "-U",
        "chamados",
        "-d",
        "chamados",
        "-c",
        "UPDATE fixture SET value='later'; CREATE TABLE stale(value text);",
    )
    shell(
        *compose,
        "exec",
        "-T",
        "chamados-backend",
        "sh",
        "-c",
        "echo later >/app/data/call-attachments/nested/test.txt; touch /app/data/call-attachments/stale",
    )
    # Corrupt bytes fail before backend stop and before any database mutation.
    key = f"/api/v4/projects/74155066/packages/generic/chamados-backup-staging/{backup_id}/backup.tar.gz"
    saved = registry[1][key]
    registry[1][key] += b"corrupt"
    failed = run("restore", backup_id)
    assert failed.returncode != 0
    assert shell(*compose, "ps", "--status", "running", "-q", "chamados-backend").stdout.strip()
    assert (
        shell(
            *compose,
            "exec",
            "-T",
            "chamados-postgresql",
            "psql",
            "-U",
            "chamados",
            "-d",
            "chamados",
            "-Atc",
            "select value from fixture",
        ).stdout.strip()
        == "later"
    )
    registry[1][key] = saved
    restored = run("restore", backup_id)
    assert restored.returncode == 0, restored.stderr
    response = json.loads(restored.stdout)
    assert response["revision"] == SHA
    assert response["data"]["safety_backup_id"] != backup_id
    assert (
        shell(
            *compose,
            "exec",
            "-T",
            "chamados-postgresql",
            "psql",
            "-U",
            "chamados",
            "-d",
            "chamados",
            "-Atc",
            "select value from fixture",
        ).stdout.strip()
        == "snapshot"
    )
    assert (
        shell(
            *compose,
            "exec",
            "-T",
            "chamados-postgresql",
            "psql",
            "-U",
            "chamados",
            "-d",
            "chamados",
            "-Atc",
            "select count(*) from pg_tables where tablename='stale'",
        ).stdout.strip()
        == "0"
    )
    assert (
        shell(*compose, "exec", "-T", "chamados-backend", "cat", "/app/data/call-attachments/nested/test.txt").stdout
        == "attachment-snapshot"
    )
    assert (
        shell(
            *compose, "exec", "-T", "chamados-backend", "test", "!", "-e", "/app/data/call-attachments/stale"
        ).returncode
        == 0
    )
    assert (deployed / ".env").read_bytes() == env_before


def test_failed_upload_reopens_backend_after_consistent_capture(stack):
    run, compose, _, registry = stack
    registry[1]["__reject_put__"] = True
    result = run("create")
    assert result.returncode != 0
    assert json.loads(result.stdout)["status"] == "failed"
    assert shell(*compose, "ps", "--status", "running", "-q", "chamados-backend").stdout.strip()
    assert not any(path.endswith("manifest.json") for method, path in registry[2] if method == "PUT")


def test_failed_health_keeps_backend_stopped_and_reports_published_safety_snapshot(stack):
    run, compose, _, registry = stack
    created = run("create")
    assert created.returncode == 0, created.stderr
    backup_id = json.loads(created.stdout)["backup_id"]
    restored = run("restore", backup_id, BACKUP_FRONTEND_HEALTH_URL="http://127.0.0.1:1/", BACKUP_HEALTH_TIMEOUT="1")
    assert restored.returncode != 0
    value = json.loads(restored.stdout)
    assert value["status"] == "failed"
    assert value["data"]["backend_stopped"] is True
    safety_id = value["data"]["safety_backup_id"]
    assert safety_id != backup_id
    assert (
        f"/api/v4/projects/74155066/packages/generic/chamados-backup-staging/{safety_id}/manifest.json" in registry[1]
    )
    assert not shell(*compose, "ps", "--status", "running", "-q", "chamados-backend").stdout.strip()
