"""Explicit local entrypoint: serialize, wait for capacity, then constrain the whole run."""

import argparse
import fcntl
import os
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "suite",
        choices=[
            "setup",
            "build",
            "smt",
            "integration",
            "ssh",
            "unit",
            "contracts",
            "checks",
            "python-checks",
            "python-fmt",
        ],
    )
    parser.add_argument("arguments", nargs="*")
    args = parser.parse_args()
    with (HERE / ".run.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        while True:
            mem = int(
                next(
                    line.split()[1]
                    for line in Path("/proc/meminfo").read_text().splitlines()
                    if line.startswith("MemAvailable:")
                )
            )
            load = os.getloadavg()[0]
            if mem >= 3 * 1024 * 1024 and load <= 4:
                break
            print(
                f"Pendente {args.suite}: load1={load:.2f}, MemAvailable={mem // 1024} MiB; aguardando 30s", flush=True
            )
            time.sleep(30)
        env = dict(
            os.environ,
            GOMAXPROCS="1",
            GOFLAGS="-p=1",
            GOMEMLIMIT="512MiB",
            COMPOSE_PARALLEL_LIMIT="1",
            PYTHONDONTWRITEBYTECODE="1",
            CHAMADOS_VERIFY_GATE_LOAD=str(load),
            CHAMADOS_VERIFY_GATE_AVAILABLE_KIB=str(mem),
        )
        prefix = [
            "rtk",
            "proxy",
            "systemd-run",
            "--user",
            "--scope",
            "--quiet",
            "-p",
            "CPUQuota=100%",
            "-p",
            "MemoryHigh=1G",
            "-p",
            "MemoryMax=1536M",
            "nice",
            "-n",
            "19",
            "ionice",
            "-c",
            "3",
        ]
        command = [sys.executable, str(HERE / "worker.py"), args.suite, *args.arguments]
        return subprocess.call(prefix + command, env=env, cwd=HERE)


if __name__ == "__main__":
    sys.exit(main())
