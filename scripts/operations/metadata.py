#!/usr/bin/env python3
"""Validated operation artifacts and GitLab calls using the current job token."""

import hashlib
import json
import os
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path


def api(path, payload=None):
    base = os.environ["CI_API_V4_URL"]
    project = urllib.parse.quote(os.environ["CI_PROJECT_ID"], safe="")
    request = urllib.request.Request(
        f"{base}/projects/{project}/{path}",
        data=None if payload is None else json.dumps(payload).encode(),
        headers={"JOB-TOKEN": os.environ["CI_JOB_TOKEN"], "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


def checked_revision(revision):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("revision must be a full commit SHA")
    return revision


def target():
    meta = json.loads(Path(".ops-target/revision.json").read_text())
    checked_revision(meta["revision"])
    if hashlib.sha256(Path(".ops-target/source.tar").read_bytes()).hexdigest() != meta["sha256"]:
        raise ValueError("source artifact checksum mismatch")
    if not isinstance(meta["ref"], str) or not meta["ref"] or meta["ref"].startswith("-"):
        raise ValueError("invalid target ref")
    return meta


def main():
    command, *args = sys.argv[1:]
    if command == "write":
        revision, ref = args
        meta = {
            "schema_version": 1,
            "revision": checked_revision(revision),
            "ref": ref,
            "sha256": hashlib.sha256(Path(".ops-target/source.tar").read_bytes()).hexdigest(),
        }
        Path(".ops-target/revision.json").write_text(json.dumps(meta) + "\n")
    elif command == "read":
        print(target()[args[0]])
    elif command == "protected-tag":
        ref, revision = args
        tag = api("repository/tags/" + urllib.parse.quote(ref, safe=""))
        if tag.get("protected") is not True or tag["commit"]["id"] != checked_revision(revision):
            raise ValueError("production tag must be protected and point to the resolved SHA")
    elif command == "result":
        status, revision = args
        result = {
            "schema_version": 1,
            "operation": os.environ.get("OPS_OPERATION", ""),
            "environment": os.environ.get("OPS_ENVIRONMENT", ""),
            "status": status,
            "revision": revision,
        }
        if Path(".ops-data.json").exists():
            data = json.loads(Path(".ops-data.json").read_text())
            result.update({key: data[key] for key in ("revision", "data") if key in data})
        Path("operation-result.json").write_text(json.dumps(result) + "\n")
    elif command == "deployment":
        result = json.loads(Path("operation-result.json").read_text())
        revision = checked_revision(result["revision"])
        ref = args[0] if args else result["data"]["ref"]
        env = os.environ["OPS_ENVIRONMENT"]
        deployment = api(
            "deployments",
            {
                "environment": f"sistema/{env}",
                "sha": revision,
                "ref": ref,
                "tag": bool(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", ref)),
                "status": "success",
            },
        )
        result["deployment_id"] = deployment["id"]
        Path("operation-result.json").write_text(json.dumps(result) + "\n")
    else:
        raise ValueError("unknown metadata command")


if __name__ == "__main__":
    main()
