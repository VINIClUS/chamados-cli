"""Explicit local SMT/Go differential round; a timeout is inconclusive, never a pass."""

import json
import subprocess
import sys
import time
from dataclasses import asdict
from pathlib import Path
from typing import Any

import fixtures
from fixtures import BACKUP, fixture
from model import FAULTS, OPERATIONS, SERVICES, Case, cases, expected_trace, oracle, path_queries


def check_terminal(result, expected):
    assert (result["status"] == "success") == expected["success"], result
    assert bool(result.get("applied")) == expected["applied"], result
    assert result["phase"] == expected["phase"], result
    assert bool(result.get("data", {}).get("mutation_started")) == expected["mutation"], result


def check_journal(f, envelope, case, expected):
    path = f.host / "operations" / (envelope["operation_id"] + ".json")
    if not expected["accepted"]:
        assert not path.exists()
        return []
    terminal = json.loads(path.read_text())["result"]
    # Both the return value and persisted state are compared with the oracle.
    check_terminal(terminal, expected)
    if OPERATIONS[case.operation] == "restore":
        assert bool(terminal["data"].get("safety_backup_id")) == expected["safety"]
        assert terminal["data"]["backend_stopped"] == expected["stopped"]
    events = sorted([*f.events(), *f.api.events], key=lambda item: item["at"])
    observations = [event["journal"] for event in events if "journal" in event]
    for observed in observations:
        assert observed["schema_version"] == 1
        assert observed["operation_id"] == envelope["operation_id"]
        assert observed["status"] == "running", observed
        if observed["operation"] == "restore" and observed["phase"] == "replace":
            assert observed["data"]["safety_backup_id"], observed
            assert observed["data"]["backend_stopped"], observed
    phases = []
    for observed in [*observations, terminal]:
        if not phases or observed["phase"] != phases[-1]:
            phases.append(observed["phase"])
    assert phases == expected_trace(case, expected), {"actual": phases, "expected": expected_trace(case, expected)}
    return phases


def exercise(case, expected):
    with fixture() as f:
        operation = OPERATIONS[case.operation]
        environment = ["staging", "production", "invalid"][case.environment]
        if operation == "restore":
            f.seed_package(environment=environment if environment != "invalid" else "staging")
        envelope = f.envelope(
            operation,
            environment=environment,
            confirm=environment if case.confirmed else "",
            service=SERVICES[case.service],
        )
        if environment == "production" and operation in {"deploy", "restore"}:
            envelope["request"]["ref"] = "v1.2.3"
            envelope["target"]["ref"] = "v1.2.3"
        if not case.ref_valid:
            envelope["request"]["ref"] = "-unsafe"
        if not case.ancestor:
            git = f.bin / "git"
            git.write_text(
                git.read_text().replace("exit 0\n", "case \" $* \" in *' merge-base '*) exit 1;; esac\nexit 0\n")
            )
        if operation == "restore":
            envelope["target"] = {}  # Host also serves direct trusted restores without runner reservation.
        f.api.protected = case.protected
        if not case.checksum:
            if operation == "deploy":
                (f.incoming / "source.tar").write_bytes(b"corrupt")
            else:
                f.api.files[f"chamados-backup-{environment}/{BACKUP}/backup.tar.gz"] = b"corrupt"
        fault = FAULTS[case.fault]
        f.save_control("attachments_replace" if fault == "attachments_replace" else fault)
        f.api.health_failure = fault == "health"
        f.api.fail_manifest = fault == "publication"
        if operation == "reset":
            # Permitted reset must actually execute; seed flag then query completes immediately.
            (f.host / ".env").write_bytes(f.env_content.replace(b"false", b"true"))
        actual = f.execute(envelope, health=f.api.url + "/api/auth/csrf")
        result = actual["result"]
        state = f.state()
        check_terminal(result, expected)
        if not expected["accepted"]:
            assert not f.events(), f.events()
            assert result["phase"] == "validate", result
        if operation == "restore":
            data = result.get("data", {})
            assert bool(data.get("safety_backup_id")) == expected["safety"], actual
            assert bool(data.get("mutation_started")) == expected["mutation"], actual
            assert state["backend"] == (not expected["stopped"]), state
            assert state["database"] == ["original", "empty", "snapshot"][expected["database"]], state
            assert state["attachments"] == ("snapshot" if expected["attachments"] else "original"), state
        if operation == "restart" and expected["success"]:
            assert state["restarted"] == (
                [SERVICES[case.service]] if case.service else ["chamados-backend", "chamados-frontend"]
            ), state
        if operation not in {"reset"}:
            assert (f.host / ".env").read_bytes() == f.env_content
        phases = check_journal(f, envelope, case, expected)
        return {
            "status": result["status"],
            "phase": result["phase"],
            "effects": state,
            "commands": len(f.events()),
            "schema_version": result["schema_version"],
            "journal_phases": phases,
        }


def main():
    start = time.monotonic()
    deadline = start + 60
    fixtures.ROUND_DEADLINE = deadline
    report: dict[str, Any] = {
        "seed": 22,
        "max_cases": 256,
        "query_timeout_ms": 2000,
        "round_seconds": 60,
        "cases": [],
        "infeasible": [],
        "inconclusive": [],
        "failures": [],
        "path_queries": path_queries(deadline),
    }
    witnesses = []
    for obligation in report["path_queries"]:
        if obligation["solver"] == "unknown":
            report["inconclusive"].append(obligation)
        elif obligation["solver"] == "unsat":
            report["infeasible"].append(obligation)
        elif obligation["goal"][0] != "phase":
            report["failures"].append({"reachability_obligation_failed": obligation})
        else:
            witnesses.append(Case(**obligation["witness"]))
    generated = list(dict.fromkeys([*witnesses, *cases()]))
    report["solver_generated_cases"] = len(set(witnesses))
    if len(generated) > 256:
        report["inconclusive"].append({"reason": "case bound", "remaining_cases": len(generated) - 256})
    for case in generated[:256]:
        if time.monotonic() >= deadline:
            report["inconclusive"].append({"reason": "round deadline", "remaining_cases": True})
            break
        expected = oracle(case, deadline)
        entry = {"case": asdict(case), "expected": expected, "solver_generated": case in witnesses}
        if expected["solver"] == "unsat":
            report["infeasible"].append(entry)
            continue
        if expected["solver"] == "unknown":
            report["inconclusive"].append(entry)
            continue
        try:
            entry["actual"] = exercise(case, expected)
            if "inconclusive" in entry["actual"]:
                report["inconclusive"].append(entry)
        except subprocess.TimeoutExpired as error:
            entry["inconclusive"] = "execution/round timeout: " + str(error)
            report["inconclusive"].append(entry)
        except Exception as error:
            entry["failure"] = str(error)
            report["failures"].append(entry)
        report["cases"].append(entry)
    report["elapsed_seconds"] = time.monotonic() - start
    output = Path(__file__).with_name("reports")
    output.mkdir(exist_ok=True)
    (output / "smt.json").write_text(json.dumps(report, indent=2))
    print(json.dumps({key: len(report[key]) for key in ["cases", "failures", "inconclusive", "infeasible"]}))
    return 1 if report["failures"] else 2 if report["inconclusive"] else 0


if __name__ == "__main__":
    sys.exit(main())
