"""Bounded independent transition oracle. Does not read executor output to predict outcomes."""

import time
from dataclasses import asdict, dataclass

import z3

OPERATIONS = ["deploy", "reset", "restore", "backup", "restart", "status", "smoke"]
SERVICES = ["", "chamados-backend", "chamados-frontend", "chamados-postgresql", "invalid"]
FAULTS = [
    "",
    "build",
    "dump",
    "replace",
    "restore_dump",
    "attachments_replace",
    "up",
    "health",
    "publication",
    "validate_dump",
]


@dataclass(frozen=True)
class Case:
    operation: int
    environment: int = 0
    confirmed: bool = True
    service: int = 0
    protected: bool = True
    ancestor: bool = True
    ref_valid: bool = True
    checksum: bool = True
    fault: int = 0


def oracle(case=None, deadline=None, goal=None):
    # The same transition system supports free path witnesses and pinned regressions.
    op, env, confirm, service, protected, checksum, fault, ancestor, ref_valid = z3.Ints(
        "op env confirm service protected checksum fault ancestor ref_valid"
    )
    accepted = z3.And(
        env < 2,
        service < 4,
        ref_valid == 1,
        z3.Not(z3.And(env == 1, z3.Or(op == 1, op == 6))),
        z3.Implies(z3.Or(op == 1, op == 2), confirm == 1),
    )
    deploy, restore, backup = op == 0, op == 2, op == 3
    reference_ok = z3.Or(env == 0, z3.And(protected == 1, ancestor == 1), z3.Not(z3.Or(deploy, restore)))
    integrity = z3.Or(checksum == 1, z3.Not(z3.Or(deploy, restore)))
    ready = z3.And(accepted, reference_ok, integrity)
    built = z3.And(ready, fault != 1, fault != 9)
    safety = z3.And(restore, built, fault != 2, fault != 8)
    replacement = z3.And(safety, fault != 3)
    database = z3.If(z3.And(replacement, fault != 4), 2, z3.If(replacement, 1, 0))
    attachments = z3.And(replacement, fault != 4, fault != 5)
    applied = z3.And(
        ready, z3.Or(z3.And(deploy, built, fault != 2, fault != 6), z3.And(restore, attachments), op == 4, op == 1)
    )
    success = z3.And(ready, fault == 0)
    stopped = z3.And(restore, built, z3.Not(success))
    mutated = z3.Or(z3.And(restore, safety), z3.And(deploy, built, fault != 2), z3.And(op == 1, ready))
    # First failing transition determines the durable phase, independently of the implementation.
    phase_guards = [
        (z3.Not(accepted), 0),
        (z3.And(restore, z3.Or(z3.Not(integrity), z3.Not(reference_ok), fault == 9)), 2),
        (z3.Not(ready), 1),
        (fault == 1, 3),
        (z3.And(restore, z3.Or(fault == 2, fault == 8)), 4),
        (z3.And(restore, fault >= 3, fault <= 6), 5),
        (z3.And(deploy, fault == 2), 6),
        (z3.And(deploy, fault == 6), 7),
        (z3.And(backup, fault != 0), 8),
        (z3.And(fault == 7, op == 4), 10),
        (fault == 7, 9),
    ]
    phase = z3.IntVal(11)
    for guard, number in reversed(phase_guards):
        phase = z3.If(guard, number, phase)
    solver = z3.Solver()
    budget = 2000 if deadline is None else min(2000, max(1, int((deadline - time.monotonic()) * 1000)))
    solver.set(timeout=budget, random_seed=22)
    variables = [op, env, confirm, service, protected, checksum, fault, ancestor, ref_valid]
    if case is not None:
        values = [
            case.operation,
            case.environment,
            int(case.confirmed),
            case.service,
            int(case.protected),
            int(case.checksum),
            case.fault,
            int(case.ancestor),
            int(case.ref_valid),
        ]
        solver.add(*[var == value for var, value in zip(variables, values)])
    solver.add(
        op >= 0, op < len(OPERATIONS), env >= 0, env <= 2, service >= 0, service <= 4, fault >= 0, fault < len(FAULTS)
    )
    for variable in [confirm, protected, checksum, ancestor, ref_valid]:
        solver.add(variable >= 0, variable <= 1)
    allowed_faults = {0: [0, 1, 2, 6, 7], 1: [0], 2: list(range(10)), 3: [0, 2, 8], 4: [0, 7], 5: [0], 6: [0]}
    for operation, faults in allowed_faults.items():
        solver.add(z3.Implies(op == operation, z3.Or(*[fault == value for value in faults])))
    if goal is not None:
        kind, value = goal
        if kind == "phase":
            solver.add(phase == value)
        elif kind == "replace_without_safety":
            solver.add(restore, mutated, z3.Not(safety))
        elif kind == "replace_corrupt_backup":
            solver.add(restore, mutated, checksum == 0)
        elif kind == "replace_without_authorization":
            solver.add(restore, mutated, z3.Not(z3.And(accepted, reference_ok)))
        else:
            raise ValueError(f"unknown path goal: {goal}")
    answer = solver.check()
    if answer != z3.sat:
        return {"solver": str(answer), "reason": solver.reason_unknown() if answer == z3.unknown else "outside domain"}
    model = solver.model()

    def boolean(expr):
        return z3.is_true(model.eval(expr, model_completion=True))

    witness = Case(
        operation=model.eval(op).as_long(),
        environment=model.eval(env).as_long(),
        confirmed=model.eval(confirm).as_long() == 1,
        service=model.eval(service).as_long(),
        protected=model.eval(protected).as_long() == 1,
        ancestor=model.eval(ancestor).as_long() == 1,
        ref_valid=model.eval(ref_valid).as_long() == 1,
        checksum=model.eval(checksum).as_long() == 1,
        fault=model.eval(fault).as_long(),
    )
    return {
        "solver": "sat",
        "witness": asdict(witness),
        "accepted": boolean(accepted),
        "success": boolean(success),
        "applied": boolean(applied),
        "safety": boolean(safety),
        "stopped": boolean(stopped),
        "mutation": boolean(mutated),
        "database": model.eval(database).as_long(),
        "attachments": boolean(attachments),
        "phase": [
            "validate",
            "prepare",
            "validate_backup",
            "build",
            "safety_backup",
            "replace",
            "pre_deploy_backup",
            "install",
            "capture",
            "health",
            "restart",
            "complete",
        ][model.eval(phase).as_long()],
    }


def cases():
    # Cover each policy boundary, then each reachable phase failure, in stable order.
    selected = set()
    for op in range(len(OPERATIONS)):
        for environment in range(3):
            for confirmed in [False, True]:
                selected.add(Case(op, environment, confirmed))
        for service in range(1, 5):
            selected.add(Case(op, service=service))
        selected.add(Case(op, ref_valid=False))
    for op in [0, 2]:
        for environment in [0, 1]:
            for protected in [False, True]:
                for checksum in [False, True]:
                    selected.add(Case(op, environment=environment, protected=protected, checksum=checksum))
        selected.add(Case(op, environment=1, ancestor=False))
    for op, faults in [(0, [1, 2, 6, 7]), (2, list(range(1, 10))), (3, [2, 8]), (4, [7])]:
        for fault in faults:
            selected.add(Case(op, fault=fault))
    return sorted(selected, key=lambda c: tuple(asdict(c).values()))


def path_queries(deadline):
    goals: list[tuple[str, int | None]] = [("phase", phase) for phase in range(12)]
    goals.extend(
        (name, None) for name in ["replace_without_safety", "replace_corrupt_backup", "replace_without_authorization"]
    )
    result = []
    for goal in goals:
        if time.monotonic() >= deadline:
            result.append({"goal": goal, "solver": "unknown", "reason": "round deadline"})
            break
        result.append({"goal": goal, **oracle(deadline=deadline, goal=goal)})
    return result


def expected_trace(case, expected):
    if not expected["accepted"]:
        return []
    paths = {
        "deploy": ["prepare", "build", "pre_deploy_backup", "install", "health"],
        "restore": ["prepare", "validate_backup", "build", "safety_backup", "replace", "health"],
        "backup": ["prepare", "capture"],
        "reset": ["prepare", "reset"],
        "restart": ["restart"],
        "status": ["status"],
        "smoke": ["smoke"],
    }
    path = paths[OPERATIONS[case.operation]]
    if expected["success"]:
        return [*path, "complete"]
    return path[: path.index(expected["phase"]) + 1]
