"""Controlled command dependency; used only via a temporary PATH entry."""

import io
import json
import subprocess
import sys
import tarfile
import time
from pathlib import Path


def classify(args):
    text = " ".join(args)
    if "dropdb" in text:
        return "replace"
    if "pg_restore" in text:
        return "validate_dump" if "--list" in args else "restore_dump"
    if args[0] == "run" and ":/snapshot" in text:
        return "attachments_replace" if "find /snapshot" in text else "attachments_capture"
    if "pg_dump" in text:
        return "dump"
    for name in ["build", "restart", "stop", "start", "up", "down"]:
        if name in args:
            return name
    return "read"


def main(control_file):
    control = json.loads(Path(control_file).read_text())
    args = sys.argv[1:]
    stage = classify(args)
    event = {"stage": stage, "args": args, "at": time.time()}
    if control.get("journal"):
        journal = Path(control["journal"])
        if journal.exists():
            event["journal"] = json.loads(journal.read_text())["result"]
    with Path(control["events"]).open("a") as out:
        out.write(json.dumps(event) + "\n")
    fault = control.get("fault", "")
    if fault == stage or (fault == "build" and stage == "build"):
        sys.exit(9)
    if control.get("real"):
        if stage == "replace" and fault == "timeout":
            args[-1] = "sleep 8; " + args[-1]
        if stage == "replace" and fault == "slow_replace":
            args[-1] = "sleep 4; " + args[-1]
        if stage == "attachments_replace" and fault == "timeout_attachments":
            args[-1] = "sleep 8; " + args[-1]
        # Every disposable helper has its own bound; Compose services sum to 480 MiB.
        if args[0] == "run":
            args[1:1] = ["--memory=64m", "--cpus=0.25", "--label", "chamados.verification.owner=" + control["project"]]
        code = subprocess.call([control["docker"], *args])
        sys.exit(code)
    state_file = Path(control["state"])
    state = json.loads(state_file.read_text())
    text = " ".join(args)
    if stage in {"stop", "start", "up", "restart"}:
        if "chamados-backend" in args or stage == "up":
            state["backend"] = stage != "stop"
    if stage == "restart":
        state["restarted"] = args[args.index("restart") + 1 :]
    if stage == "replace":
        state["database"] = "empty"
    if stage == "restore_dump":
        sys.stdin.buffer.read()
        state["database"] = "snapshot"
    if stage == "attachments_replace":
        sys.stdin.buffer.read()
        state["attachments"] = "snapshot"
    state_file.write_text(json.dumps(state))
    if stage == "dump":
        sys.stdout.buffer.write(b"PGDMPcontrolled-database")
    elif stage == "attachments_capture":
        with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as tar:
            member = tarfile.TarInfo("fixture.txt")
            member.size = 8
            tar.addfile(member, io.BytesIO(b"original"))
    elif "SHOW server_version_num" in text:
        print("160000")
    elif "select count(*) from users" in text:
        print("1")
    elif args[0] == "inspect":
        print("postgres:16-alpine")
    elif "ps" in args:
        print("controlled-container")
    elif "validate_dump" == stage:
        sys.stdin.buffer.read()
    elif "version" in args:
        print("Docker Compose version local")


if __name__ == "__main__":
    main(str(Path(__file__).with_name("control.json")))
