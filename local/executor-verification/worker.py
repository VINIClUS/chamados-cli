"""Runs inside run.py's scope. No automatic discovery by existing jobs."""

import json
import os
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def call(*command, cwd=HERE):
    subprocess.run(["rtk", "proxy", *map(str, command)], cwd=cwd, check=True)


def build_binaries():
    (HERE / "bin").mkdir(exist_ok=True)
    call("go", "build", "-o", HERE / "bin/probe", ".")
    call("go", "build", "-tags=netgo", "-o", HERE / "bin/httpd", "./httpd")
    call(
        "go",
        "build",
        "-ldflags=-X main.commit=" + "a" * 40,
        "-o",
        HERE / "bin/chamadosctl",
        "./cmd/chamadosctl",
        cwd=ROOT,
    )


def main():
    suite = sys.argv[1]
    reports = HERE / "reports"
    reports.mkdir(exist_ok=True)
    cgroup = Path("/proc/self/cgroup").read_text().strip().split(":", 2)[-1]
    scope = Path("/sys/fs/cgroup") / cgroup.lstrip("/")
    metadata = {
        "suite": suite,
        "verification_class": os.environ.get("CHAMADOS_VERIFY_CLASS", "capacity-gated"),
        "at": time.time(),
        "cgroup": cgroup,
        "load1_at_gate": os.environ.get("CHAMADOS_VERIFY_GATE_LOAD"),
        "available_kib_at_gate": os.environ.get("CHAMADOS_VERIFY_GATE_AVAILABLE_KIB"),
        "cpu_max": (scope / "cpu.max").read_text().strip(),
        "memory_high": (scope / "memory.high").read_text().strip(),
        "memory_max": (scope / "memory.max").read_text().strip(),
        "nice": os.getpriority(os.PRIO_PROCESS, 0),
        "io_priority": subprocess.check_output(["rtk", "proxy", "ionice", "-p", str(os.getpid())]).decode().strip(),
    }
    assert metadata["memory_max"] == "1610612736" and metadata["memory_high"] == "1073741824"
    quota, period = map(int, metadata["cpu_max"].split())
    assert quota == period and metadata["nice"] == 19
    with (reports / "runs.jsonl").open("a") as out:
        out.write(json.dumps(metadata) + "\n")
    python = HERE / ".venv/bin/python"
    if suite == "setup":
        call(sys.executable, "-m", "venv", HERE / ".venv")
        call(python, "-m", "pip", "install", "--disable-pip-version-check", "-r", HERE / "requirements.txt")
    if suite in {"setup", "build"}:
        build_binaries()
    elif suite == "unit":
        call("go", "test", "-p=1", "-parallel=1", "./...", cwd=ROOT)
    elif suite == "contracts":
        call(python, "-m", "pytest", "-q", "tests", cwd=ROOT)
        scripts = [
            *sorted((ROOT / "scripts/cli").glob("*.sh")),
            *sorted((ROOT / "scripts/operations").glob("*.sh")),
            ROOT / "scripts/deploy.sh",
            ROOT / "scripts/deploy/remote-deploy.sh",
            ROOT.parent / "infrastructure/scripts/setup-runner.sh",
        ]
        call("shellcheck", *scripts, cwd=ROOT)
    elif suite == "checks":
        call("go", "vet", "./...", cwd=ROOT)
        call("go", "test", "-race", "-p=1", "-parallel=1", "./...", cwd=ROOT)
        quality = subprocess.run(
            ["rtk", "proxy", "bash", "scripts/cli/quality.sh", "check"], cwd=ROOT, capture_output=True
        )
        (reports / "quality.log").write_bytes(quality.stdout + quality.stderr)
        status = (ROOT / ".quality/status.txt").read_text()
        issues = json.loads((ROOT / ".quality/gl-code-quality-report.json").read_text())
        summary = {"exit_code": quality.returncode, "status": status, "advisory_findings": len(issues)}
        (reports / "quality.json").write_text(json.dumps(summary, indent=2))
        print(json.dumps(summary))
        assert "format=0" in status, "required formatting check failed; lint remains advisory"
    elif suite in {"python-checks", "python-fmt"}:
        ruff = HERE / ".venv/bin/ruff"
        ty = HERE / ".venv/bin/ty"
        if suite == "python-fmt":
            call(ruff, "format", ROOT)
            call(ruff, "check", "--fix", ROOT)
            call(ruff, "format", ROOT)
        else:
            call(ruff, "check", ROOT)
            call(ruff, "format", "--check", ROOT)
        typed = subprocess.run(
            [
                "rtk",
                "proxy",
                str(ty),
                "check",
                "--python",
                str(HERE / ".venv/bin/python"),
                "--extra-search-path",
                str(ROOT / "tests"),
                "--output-format",
                "concise",
                str(ROOT),
            ],
            capture_output=True,
        )
        (reports / "ty.log").write_bytes(typed.stdout + typed.stderr)
        print(typed.stdout.decode() + typed.stderr.decode())
        typed.check_returncode()
        versions = {
            "ruff": subprocess.check_output(["rtk", "proxy", str(ruff), "--version"]).decode().strip(),
            "ty": subprocess.check_output(["rtk", "proxy", str(ty), "--version"]).decode().strip(),
        }
        if suite == "python-checks":
            (reports / "python-checks.json").write_text(
                json.dumps({**versions, "lint": "passed", "format": "passed", "types": "passed"}, indent=2)
            )
    else:
        build_binaries()
        if suite == "ssh":
            call(python, HERE / "integration.py", "ssh")
        else:
            call(python, HERE / (suite + ".py"), *sys.argv[2:])


if __name__ == "__main__":
    main()
