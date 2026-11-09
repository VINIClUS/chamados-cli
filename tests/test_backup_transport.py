import json
import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/operations/backup.sh"


def test_unconfirmed_restore_never_calls_ssh_and_writes_failed_result(tmp_path):
    tools = tmp_path / "bin"
    tools.mkdir()
    marker = tmp_path / "ssh-called"
    (tools / "ssh").write_text(f"#!/bin/sh\ntouch '{marker}'\nexit 98\n")
    (tools / "ssh").chmod(0o755)
    result = subprocess.run(
        ["bash", str(SCRIPT), "restore", "production", "20261107120000.42"],
        env={**os.environ, "PATH": str(tools) + ":" + os.environ["PATH"], "OPS_CONFIRM": "staging"},
        cwd=tmp_path,
        capture_output=True,
        text=True,
    )
    assert result.returncode != 0
    assert not marker.exists()
    value = json.loads((tmp_path / "operation-result.json").read_text())
    assert value["status"] == "failed"
    assert value["environment"] == "production"
