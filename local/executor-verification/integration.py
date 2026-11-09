"""Explicit serial integration. Uses only loopback, unique temporary state and owned Docker resources."""

import fcntl
import hashlib
import io
import json
import shutil
import sys
import tarfile
import time
import traceback
from datetime import UTC, datetime, timedelta

from compose import Compose
from fixtures import BACKUP, HERE, SHA, command, fixture, write_json
from ssh_local import LocalSSH


def successful(output):
    assert output["error"] == "", output
    assert output["result"]["status"] == "success", output
    return output["result"]


def tar_bytes(tar: tarfile.TarFile, name: str) -> bytes:
    member = tar.extractfile(name)
    assert member is not None, f"archive member is not a regular file: {name}"
    with member:
        return member.read()


def restore_envelope(f, backup_id):
    env = f.envelope("restore", backup_id=backup_id)
    env["target"] = {}
    return env


def safety_exists(f, result):
    assert result.get("data", {}).get("safety_backup_id"), result
    backup_id = result["data"]["safety_backup_id"]
    key = f"chamados-backup-staging/{backup_id}/"
    assert key + "manifest.json" in f.api.files, result
    assert key + "backup.tar.gz" in f.api.files, result
    return backup_id


def roundtrip(f, compose, report):
    backup = successful(f.execute(f.envelope("backup")))
    backup_id = backup["backup_id"]
    key = f"chamados-backup-staging/{backup_id}/"
    external = json.loads(f.api.files[key + "manifest.json"])
    payload = f.api.files[key + "backup.tar.gz"]
    assert external["sha256"] == hashlib.sha256(payload).hexdigest()
    with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as tar:
        assert set(tar.getnames()) == {"manifest.json", "database.dump", "attachments.tar"}
        internal = json.loads(tar_bytes(tar, "manifest.json"))
        assert internal["schema_version"] == 1
        for member in external["members"]:
            content = tar_bytes(tar, member["name"])
            assert len(content) == member["size"]
            assert hashlib.sha256(content).hexdigest() == member["sha256"]
    compose.sql("UPDATE marker SET value='changed'")
    compose.call("exec", "-T", "chamados-backend", "sh", "-c", "printf changed > /attachments/fixture.txt")
    result = successful(f.execute(restore_envelope(f, backup_id)))
    assert compose.sql("SELECT value FROM marker") == "snapshot"
    assert compose.attachment() == "snapshot"
    assert compose.backend_running()
    assert (f.host / ".env").read_bytes() == f.env_content
    safety = safety_exists(f, result)
    report.append(
        {"name": "database_attachment_roundtrip_manifest_env", "backup_id": backup_id, "safety_backup_id": safety}
    )
    return backup_id


def corrupt_and_partial(f, compose, backup_id, report):
    key = f"chamados-backup-staging/{backup_id}/backup.tar.gz"
    original = f.api.files[key]
    f.api.files[key] = original[:-1] + bytes([original[-1] ^ 1])
    before = len(f.events())
    output = f.execute(restore_envelope(f, backup_id))
    assert output["error"] and output["result"]["phase"] == "validate_backup", output
    assert not any(
        e["stage"] in {"replace", "restore_dump", "attachments_replace", "stop"} for e in f.events()[before:]
    )
    assert compose.sql("SELECT value FROM marker") == "snapshot"
    assert compose.attachment() == "snapshot"
    assert compose.backend_running()
    f.api.files[key] = original
    before = set(f.api.files)
    f.api.fail_manifest = True
    output = f.execute(f.envelope("backup"))
    assert output["error"] and output["result"]["status"] == "failed", output
    added = set(f.api.files) - before
    assert len(added) == 1 and next(iter(added)).endswith("backup.tar.gz"), added
    assert compose.backend_running()
    f.api.fail_manifest = False
    report.append({"name": "corruption_no_data_change_and_incomplete_publication"})


def restarts_and_locks(f, compose, report):
    database = compose.call("ps", "-q", "chamados-postgresql").stdout.decode().strip()
    started = command(f.control["docker"], "inspect", "-f", "{{.State.StartedAt}}", database).stdout
    for service in ["", "chamados-backend", "chamados-frontend"]:
        successful(f.execute(f.envelope("restart", service=service)))
        event = [e for e in f.events() if e["stage"] == "restart"][-1]
        actual = event["args"][event["args"].index("restart") + 1 :]
        assert actual == ([service] if service else ["chamados-backend", "chamados-frontend"])
        assert command(f.control["docker"], "inspect", "-f", "{{.State.StartedAt}}", database).stdout == started
    before = len(f.events())
    with (f.host / ".deploy.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        output = f.execute(f.envelope("restart"))
        assert output["error"], output
    assert len(f.events()) == before
    env = f.envelope("status")
    successful(f.execute(env))
    before = len(f.events())
    successful(f.execute(env))
    assert len(f.events()) == before, "terminal operation replayed"
    env["request"]["ref"] = "changed"
    assert f.execute(env)["error"], "conflicting replay accepted"
    assert len(f.events()) == before
    report.append({"name": "selective_restart_lock_and_replay"})


def restore_failures(f, compose, backup_id, report):
    compose.sql("UPDATE marker SET value='before-failed-restore'")
    f.save_control("attachments_replace")
    output = f.execute(restore_envelope(f, backup_id))
    result = output["result"]
    assert output["error"] and result["data"]["mutation_started"], output
    assert not compose.backend_running(), "backend reopened after partial restore"
    assert compose.sql("SELECT value FROM marker") == "snapshot"
    safety = safety_exists(f, result)
    f.save_control("")
    compose.call("start", "chamados-backend")
    f.api.health_failure = True
    output = f.execute(restore_envelope(f, backup_id))
    result = output["result"]
    assert output["error"] and result["applied"] and result["phase"] == "health", output
    assert not compose.backend_running()
    safety_exists(f, result)
    f.api.health_failure = False
    compose.call("start", "chamados-backend")
    report.append({"name": "failure_after_replacement_and_after_activation", "safety_backup_id": safety})


def daemon_timeout(f, compose, backup_id, report, attachment=False):
    compose.sql("UPDATE marker SET value='must-survive-timeout'")
    compose.call("exec", "-T", "chamados-backend", "sh", "-c", "printf must-survive-timeout > /attachments/fixture.txt")
    fault = "timeout_attachments" if attachment else "timeout"
    f.save_control(fault)
    env = restore_envelope(f, backup_id)
    env["deadline"] = (datetime.now(UTC) + timedelta(seconds=6)).isoformat()
    output = f.execute(env)
    result = output["result"]
    timed_out_at = time.time()
    assert output["error"] and result["error"]["code"] == "timeout", output
    assert not compose.backend_running(), "backend must stay stopped on timeout"
    safety = safety_exists(f, result)
    # Wait beyond the daemon's injected delay, then start only PostgreSQL if cancellation fenced it.
    time.sleep(9)
    f.save_control("")
    compose.call("up", "-d", "--wait", "chamados-postgresql")
    if attachment:
        assert compose.attachment() == "must-survive-timeout", "late helper replaced attachments after timeout"
    else:
        assert compose.sql("SELECT value FROM marker") == "must-survive-timeout", (
            "late daemon exec replaced database after timeout"
        )
    assert not compose.backend_running()
    compose.call("start", "chamados-backend")
    report.append(
        {
            "name": "daemon_timeout_attachments" if attachment else "daemon_timeout_database",
            "safety_backup_id": safety,
            "timed_out_at": timed_out_at,
            "verified_no_late_mutation_at": time.time(),
            "backend_stopped": True,
        }
    )


def retention(f, compose, backup_id, report):
    old_valid = "20240101000000.9"
    old_broken = "20230101000000.8"
    recent_broken = "20261107000000.7"
    for name in ["backup.tar.gz", "manifest.json"]:
        f.api.files[f"chamados-backup-staging/{old_valid}/{name}"] = f.api.files[
            f"chamados-backup-staging/{backup_id}/{name}"
        ]
    now = datetime.now(UTC)
    f.api.packages = [
        {
            "id": 1,
            "name": "chamados-backup-staging",
            "version": recent_broken,
            "package_type": "generic",
            "created_at": (now - timedelta(days=5)).isoformat(),
        },
        {
            "id": 2,
            "name": "chamados-backup-staging",
            "version": old_valid,
            "package_type": "generic",
            "created_at": (now - timedelta(days=60)).isoformat(),
        },
        {
            "id": 3,
            "name": "chamados-backup-staging",
            "version": old_broken,
            "package_type": "generic",
            "created_at": (now - timedelta(days=80)).isoformat(),
        },
    ]
    successful(f.execute(f.envelope("cleanup")))
    deleted = [e["path"].rsplit("/", 1)[-1] for e in f.api.events if e["method"] == "DELETE"]
    assert deleted == ["3"], deleted
    # Seed six older SQL dumps; a real deploy creates the seventh and retains exactly five.
    backups = f.host / "backups"
    backups.mkdir(exist_ok=True)
    for n in range(6):
        (backups / f"pre-deploy-2000010100000{n}-fixture.sql.gz").write_bytes(b"older fixture")
    result = successful(f.execute(f.envelope("deploy")))
    files = sorted(backups.glob("pre-deploy-*.sql.gz"))
    assert len(files) == 5, files
    import gzip

    assert b"CREATE TABLE" in gzip.decompress(files[-1].read_bytes())
    assert result["applied"]
    assert (f.host / ".env").read_bytes() == f.env_content
    report.append({"name": "retention_30_days_last_valid_and_five_predeploy_SQL_dumps"})


def prepare_runner(f):
    cwd = f.root / "runner" / "devops"
    (cwd / ".ops-executor").mkdir(parents=True, exist_ok=True)
    (cwd / ".ops-target").mkdir(exist_ok=True)
    infrastructure = cwd.parent / "infrastructure"
    (infrastructure / "environments").mkdir(parents=True, exist_ok=True)
    (infrastructure / "scripts").mkdir(parents=True, exist_ok=True)
    binary = cwd / ".ops-executor/chamadosctl"
    shutil.copy2(HERE / "bin/chamadosctl", binary)
    identity = json.loads(command(binary, "internal", "inspect", "--binary").stdout)
    write_json(cwd / ".ops-executor/manifest.json", identity)
    (cwd / ".ops-executor/SHA256SUMS").write_text(identity["sha256"] + "  chamadosctl\n")
    for name in ["docker-compose.staging.yml", "docker-compose.production.yml"]:
        (infrastructure / "environments" / name).write_text("services: {}\n# trusted overlay\n")
    (infrastructure / "scripts/bootstrap-host.sh").write_text("#!/bin/sh\nexit 89\n")
    shutil.copy2(f.incoming / "source.tar", cwd / ".ops-target/source.tar")
    write_json(cwd / ".ops-target/revision.json", f.target)
    return cwd


def origin_separation(f, report):
    repository = f.root / "application-origin"
    repository.mkdir()
    command("git", "init", "-b", "main", cwd=repository)
    command("git", "config", "user.email", "local@example.invalid", cwd=repository)
    command("git", "config", "user.name", "Local fixture", cwd=repository)
    for name, content in f.files.items():
        path = repository / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
    command("git", "add", ".", cwd=repository)
    command("git", "commit", "-qm", "protected application base", cwd=repository)
    command("git", "checkout", "-b", "feature/application-only", cwd=repository)
    for name in [
        "devops/scripts/deploy/bootstrap-host.sh",
        "devops/scripts/deploy/docker-compose.staging.yml",
        "scripts/deploy/bootstrap-host.sh",
        "scripts/deploy/docker-compose.staging.yml",
        "internal/executor.go",
        "cicd/pipeline.yml",
        "infrastructure/environments/docker-compose.staging.yml",
        "infrastructure/scripts/bootstrap-host.sh",
        ".env",
        ".gitlab/evil.yml",
    ]:
        path = repository / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("untrusted requested source must never become executor tooling")
    (repository / "marker").write_text("application-from-OPS_REF")
    # The backend tests need the contract export script, the only file kept from scripts/.
    (repository / "scripts/export-openapi.sh").write_text("#!/bin/sh\n")
    (repository / "scripts/export-openapi.sh").chmod(0o755)
    command("git", "add", ".", cwd=repository)
    command("git", "commit", "-qm", "untrusted application ref", cwd=repository)
    revision = command("git", "rev-parse", "HEAD", cwd=repository).stdout.decode().strip()
    cwd = prepare_runner(f)
    command("git", "init", cwd=cwd)
    command("git", "remote", "add", "origin", repository, cwd=cwd)
    env = dict(
        f.env,
        CI_COMMIT_BRANCH="main",
        CI_COMMIT_REF_PROTECTED="true",
        CI_COMMIT_SHA=SHA,
        CI_API_V4_URL=f.api.url + "/api/v4",
        CI_PROJECT_ID="42",
        CI_JOB_TOKEN="local-only",
        OPS_OPERATION="deploy",
        OPS_ENVIRONMENT="staging",
        OPS_REF="feature/application-only",
    )
    command(HERE / "bin/chamadosctl", "internal", "resolve", cwd=cwd, env=env)
    target = json.loads((cwd / ".ops-target/revision.json").read_text())
    assert target["revision"] == revision and target["ref"] == env["OPS_REF"]
    with tarfile.open(cwd / ".ops-target/source.tar") as tar:
        names = tar.getnames()
        assert not any(
            name == ".env"
            or (
                name.startswith(
                    ("scripts/", ".gitlab/", "devops/", "cicd/", "infrastructure/environments/", "infrastructure/scripts/")
                )
                and name != "scripts/export-openapi.sh"
            )
            for name in names
        ), names
        assert tar.getmember("scripts/export-openapi.sh").mode & 0o100
        assert tar_bytes(tar, "marker") == b"application-from-OPS_REF"
    identity = json.loads((cwd / ".ops-executor/manifest.json").read_text())
    assert identity["executor_sha"] == SHA
    assert "trusted overlay" in (cwd.parent / "infrastructure/environments/docker-compose.staging.yml").read_text()
    # Unprotected pipeline must be rejected before local git fetch or artifact replacement.
    target_before = (cwd / ".ops-target/revision.json").read_bytes()
    refused = command(
        HERE / "bin/chamadosctl",
        "internal",
        "resolve",
        cwd=cwd,
        env=dict(env, CI_COMMIT_REF_PROTECTED="false"),
        check=False,
    )
    assert refused.returncode and (cwd / ".ops-target/revision.json").read_bytes() == target_before
    report.append(
        {
            "name": "protected_executor_and_overlays_OPS_REF_only_application",
            "application_sha": revision,
            "executor_sha": SHA,
        }
    )
    # prepare_runner resets the target for subsequent independent transport checks.


def runner(f, cwd, operation, backup_id="", **extra):
    env = dict(
        f.env,
        CI_COMMIT_BRANCH="main",
        CI_COMMIT_REF_PROTECTED="true",
        CI_COMMIT_SHA=SHA,
        CI_PIPELINE_ID="200",
        CI_JOB_ID=str(f.counter + 1000),
        CI_PIPELINE_IID=str(f.counter + 1000),
        CI_JOB_TOKEN="local-only",
        CI_PROJECT_ID="42",
        CI_PROJECT_PATH="local/fixture",
        CI_API_V4_URL=f.api.url + "/api/v4",
        DEPLOY_URL=f.api.url,
        DEPLOY_EDGE_IP="",
        DEPLOY_SSH="fixture@127.0.0.1",
        DEPLOY_SSH_KEY=str(f.root / "client-key"),
        OPS_OPERATION=operation,
        OPS_ENVIRONMENT="staging",
        OPS_REF="main",
        OPS_CONFIRM="staging",
        OPS_BACKUP_ID=backup_id,
    )
    env.update(extra)
    f.counter += 1
    output = command(HERE / "bin/chamadosctl", "internal", "execute", cwd=cwd, env=env, check=False, timeout=55)
    try:
        result = json.loads(output.stdout)
    except ValueError:
        raise AssertionError(output.stderr.decode())
    assert json.loads((cwd / "operation-result.json").read_text()) == result
    if output.returncode:
        print(
            "runner result",
            json.dumps(result),
            "diagnostics",
            output.stderr.decode(errors="replace")[-2500:],
            flush=True,
        )
    return result, output.returncode


def deployment_and_ssh(f, compose, backup_id, report):
    cwd = prepare_runner(f)
    ssh = LocalSSH(f)
    try:
        # A response with no ID is uncertain even if the server accepted POST: never retry or reach SSH.
        f.api.post_uncertain = True
        before = len(ssh.calls)
        result, code = runner(f, cwd, "deploy")
        assert code and result["error"]["code"] == "deployment_create_uncertain", result
        assert len(ssh.calls) == before
        posts = [e for e in f.api.events if e["method"] == "POST" and e["path"].endswith("/deployments")]
        assert len(posts) == 1 and posts[0]["body"]["status"] == "running"
        f.api.post_uncertain = False
        f.api.put_failure = True
        before_events = len(f.events())
        result, code = runner(f, cwd, "restore", backup_id)
        assert code and result.get("applied") and result.get("deployment_id") == 17, result
        assert result["error"]["code"] == "deployment_update_pending", result
        safety_exists(f, result)
        puts = [e for e in f.api.events if e["method"] == "PUT" and "/deployments/" in e["path"]]
        assert len(puts) == 3 and all(e["path"].endswith("/17") for e in puts), puts
        reservation = [e for e in f.api.events if e["method"] == "POST"][-1]
        replacement = next(e for e in f.events()[before_events:] if e["stage"] == "replace")
        assert reservation["at"] < replacement["at"]
        # POST is observable before any execute channel, and requests use application SHA, not executor SHA.
        assert all(e["body"]["status"] == "running" for e in f.api.events if e["method"] == "POST")
        f.api.put_failure = False
        f.save_control("slow_replace")
        ssh.drop = "running"
        before = sum("execute --remote" in call for call in ssh.calls)
        result, code = runner(f, cwd, "restore", backup_id)
        assert code and result["error"]["code"] == "state_unknown", result
        safety_exists(f, result)
        assert sum("execute --remote" in call for call in ssh.calls) == before + 1
        assert any("internal inspect '" in call for call in ssh.calls), ssh.calls
        # Let the already-started operation finish; recovering the channel never reruns it.
        for process in ssh.processes:
            process.wait(timeout=40)
        f.save_control("")
        ssh.drop = ""
        report.append(
            {
                "name": "verified_local_ssh_deployment_uncertainty_bounded_put_and_journal_recovery",
                "post_attempts": len([e for e in f.api.events if e["method"] == "POST"]),
                "put_attempts_on_17": len(puts),
                "safety_backup_id": result["data"]["safety_backup_id"],
            }
        )
    finally:
        ssh.close()


def terminal_channel_recovery(report):
    # Real SSH/CLI and journal, controlled operation dependencies: exercise the
    # identical reconciliation path for all three non-repeatable operations.
    with fixture() as f:
        f.seed_package()
        (f.host / ".env").write_bytes(f.env_content.replace(b"false", b"true"))
        cwd = prepare_runner(f)
        ssh = LocalSSH(f)
        ssh.drop = "terminal"
        try:
            known = f.root / "known_hosts"
            original_known = known.read_text()
            address = original_known.split()[0]
            known.write_text(f"{address} {ssh.client_key.get_name()} {ssh.client_key.get_base64()}\n")
            before = len(ssh.calls)
            refused, code = runner(f, cwd, "deploy")
            assert code and len(ssh.calls) == before, "untrusted SSH host key reached execution"
            known.write_text(original_known)
            for operation in ["deploy", "reset", "restore"]:
                before = len(ssh.calls)
                result, code = runner(f, cwd, operation, BACKUP)
                assert code == 0 and result["status"] == "success", result
                calls = ssh.calls[before:]
                assert sum("execute --remote" in call for call in calls) == 1, calls
                assert sum("internal inspect '" in call for call in calls) == 1, calls
            ssh.inspect_failure = True
            before = len(ssh.calls)
            result, code = runner(f, cwd, "restore", BACKUP)
            assert code and result["error"]["code"] == "state_unknown" and result["applied"], result
            safety_exists(f, result)
            assert sum("execute --remote" in call for call in ssh.calls[before:]) == 1
            report.append(
                {
                    "name": "lost_SSH_deploy_restore_reset_never_replayed_and_last_terminal_retained",
                    "dependency_mode": "controlled; SSH, CLI and journal real",
                    "untrusted_host_key_refused": True,
                    "safety_backup_id": result["data"]["safety_backup_id"],
                }
            )
        finally:
            ssh.close()


def main():
    report = {
        "cases": [],
        "failures": [],
        "limits": {"compose_mib": 480, "helper_mib": 64, "max_total_mib": 544, "parallel": 1},
    }
    with fixture(real=True) as f:
        compose = Compose(f)
        try:
            compose.start()
            ids = compose.call("ps", "-q").stdout.decode().split()
            limits = json.loads(command(f.control["docker"], "inspect", *ids).stdout)
            actual_limits = [
                {"name": c["Name"], "memory": c["HostConfig"]["Memory"], "nano_cpus": c["HostConfig"]["NanoCpus"]}
                for c in limits
            ]
            assert sum(c["memory"] for c in actual_limits) == 480 * 1024 * 1024
            assert all(c["nano_cpus"] > 0 for c in actual_limits)
            report["container_limits"] = actual_limits
            backup_id = roundtrip(f, compose, report["cases"])
            for name, action in [
                ("corrupt_partial", lambda: corrupt_and_partial(f, compose, backup_id, report["cases"])),
                ("restart_locks", lambda: restarts_and_locks(f, compose, report["cases"])),
                ("restore_failure", lambda: restore_failures(f, compose, backup_id, report["cases"])),
                ("origin_separation", lambda: origin_separation(f, report["cases"])),
                ("ssh_deployment", lambda: deployment_and_ssh(f, compose, backup_id, report["cases"])),
                ("timeout_database", lambda: daemon_timeout(f, compose, backup_id, report["cases"])),
                (
                    "timeout_attachments",
                    lambda: daemon_timeout(f, compose, backup_id, report["cases"], attachment=True),
                ),
                ("retention", lambda: retention(f, compose, backup_id, report["cases"])),
            ]:
                try:
                    action()
                    print("PASS", name, flush=True)
                except Exception as error:
                    report["failures"].append({"name": name, "error": str(error), "trace": traceback.format_exc()})
                    print("FAIL", name, str(error), flush=True)
                    # Restore usable fixture without altering evidence of failure.
                    f.save_control("")
                    f.api.health_failure = f.api.fail_manifest = f.api.put_failure = False
                    compose.call("up", "-d", "--wait")
                    compose.sql(
                        "CREATE TABLE IF NOT EXISTS marker(value text); DELETE FROM marker; INSERT INTO marker VALUES ('snapshot')"
                    )
        except Exception as error:
            report["failures"].append({"name": "setup_roundtrip", "error": str(error), "trace": traceback.format_exc()})
        finally:
            try:
                compose.close()
                report["cleanup"] = "owned resources removed"
            except Exception as error:
                report["failures"].append({"name": "cleanup", "error": str(error)})
            report["commands"] = f.events()
            report["http"] = f.api.events
    try:
        terminal_channel_recovery(report["cases"])
    except Exception as error:
        report["failures"].append(
            {"name": "terminal_channel_recovery", "error": str(error), "trace": traceback.format_exc()}
        )
    output = HERE / "reports"
    output.mkdir(exist_ok=True)
    (output / "integration.json").write_text(json.dumps(report, indent=2))
    print(
        json.dumps(
            {"cases": len(report["cases"]), "failures": len(report["failures"]), "cleanup": report.get("cleanup")}
        )
    )
    return bool(report["failures"])


def ssh_main():
    report = {
        "scope": "terminal SSH reconciliation with controlled operation dependencies",
        "cases": [],
        "failures": [],
    }
    try:
        terminal_channel_recovery(report["cases"])
    except Exception as error:
        report["failures"].append(
            {"name": "terminal_channel_recovery", "error": str(error), "trace": traceback.format_exc()}
        )
    (HERE / "reports/ssh.json").write_text(json.dumps(report, indent=2))
    print(json.dumps({"cases": len(report["cases"]), "failures": len(report["failures"])}))
    return bool(report["failures"])


if __name__ == "__main__":
    sys.exit(ssh_main() if sys.argv[1:] == ["ssh"] else main())
