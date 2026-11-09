"""A unique Compose project: PostgreSQL 16 + two disposable HTTP services, total 480 MiB."""

import json
import socket
import time
import urllib.request

from fixtures import HERE, command


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Compose:
    def __init__(self, fixture):
        self.fixture = fixture
        self.backend_port, self.frontend_port = unused_port(), unused_port()
        while self.backend_port == self.frontend_port:
            self.frontend_port = unused_port()
        services = {}
        for name, port in [("backend", self.backend_port), ("frontend", self.frontend_port)]:
            services["chamados-" + name] = {
                "image": "postgres:16-alpine",
                "mem_limit": "48m",
                "cpus": 0.15,
                "entrypoint": ["/local-httpd"],
                "environment": {"GOMAXPROCS": "1", "GOMEMLIMIT": "32MiB"},
                "ports": [f"127.0.0.1:{port}:8080"],
                "volumes": [str(HERE / "bin/httpd") + ":/local-httpd:ro"]
                + (["call_attachments:/attachments"] if name == "backend" else []),
            }
        services["chamados-postgresql"] = {
            "image": "postgres:16-alpine",
            "mem_limit": "384m",
            "cpus": 0.5,
            "environment": {"POSTGRES_USER": "fixture", "POSTGRES_PASSWORD": "fixture", "POSTGRES_DB": "fixture"},
            "volumes": ["postgres_data:/var/lib/postgresql/data"],
            "healthcheck": {
                "test": ["CMD-SHELL", "pg_isready -U fixture"],
                "interval": "1s",
                "timeout": "1s",
                "retries": 30,
            },
        }
        content = json.dumps({"services": services, "volumes": {"postgres_data": {}, "call_attachments": {}}}).encode()
        backend = fixture.files.pop("Dockerfile")
        frontend = fixture.files.pop("src/web/Dockerfile")
        fixture.files.pop("docker-compose.yml")
        fixture.files["infrastructure/application/backend.Dockerfile"] = backend
        fixture.files["infrastructure/application/frontend.Dockerfile"] = frontend
        fixture.files["infrastructure/application/docker-compose.yml"] = content
        fixture.set_source()
        application = fixture.host / "app/infrastructure/application"
        application.mkdir(parents=True)
        (application / "docker-compose.yml").write_bytes(content)
        self.compose_file = fixture.root / "owned-compose.json"
        self.compose_file.write_bytes(content)
        for base in [fixture.host / "tooling/deploy", fixture.incoming / "tooling/deploy"]:
            for environment in ["staging", "production"]:
                (base / f"docker-compose.{environment}.yml").write_text("services: {}\n")
        self.base = [fixture.control["docker"], "compose", "-p", fixture.project, "-f", self.compose_file]

    def call(self, *args, **kwargs):
        return command(*self.base, *args, **kwargs)

    def start(self):
        # No implicit pull; nothing from the application image is executed here.
        command(self.fixture.control["docker"], "image", "inspect", "postgres:16-alpine")
        self.call("up", "-d", "--wait", "--pull", "never", timeout=55)
        self.sql("CREATE TABLE marker(value text); INSERT INTO marker VALUES ('snapshot');")
        self.call("exec", "-T", "chamados-backend", "sh", "-c", "printf snapshot > /attachments/fixture.txt")
        self.wait_http()

    def sql(self, query):
        return (
            self.call("exec", "-T", "chamados-postgresql", "psql", "-U", "fixture", "-d", "fixture", "-Atc", query)
            .stdout.decode()
            .strip()
        )

    def attachment(self):
        # Backend can be stopped after an error; use a bounded read-only helper.
        return command(
            self.fixture.control["docker"],
            "run",
            "--rm",
            "--memory=64m",
            "--cpus=0.25",
            "--label",
            "chamados.verification.owner=" + self.fixture.project,
            "--network=none",
            "--read-only",
            "-v",
            self.fixture.project + "_call_attachments:/snapshot:ro",
            "--entrypoint",
            "cat",
            "postgres:16-alpine",
            "/snapshot/fixture.txt",
        ).stdout.decode()

    def backend_running(self):
        return bool(self.call("ps", "-q", "--status", "running", "chamados-backend").stdout.strip())

    def wait_http(self):
        for port in [self.backend_port, self.frontend_port]:
            deadline = time.monotonic() + 10
            while True:
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}", timeout=1) as response:
                        assert response.status == 200
                    break
                except OSError:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.1)

    def close(self):
        self.call("down", "-v", "--remove-orphans", timeout=55)
        docker = self.fixture.control["docker"]
        owned = (
            command(docker, "ps", "-aq", "--filter", "label=chamados.verification.owner=" + self.fixture.project)
            .stdout.decode()
            .split()
        )
        if owned:
            command(docker, "rm", "-f", *owned)
        assert not self.call("ps", "-aq").stdout.strip(), "owned project leaked containers"
