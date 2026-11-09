"""Regression checks for verifier subprocess isolation."""

import os
import tempfile
from pathlib import Path

from fixtures import command


def test_command_clears_inherited_git_repository_variables():
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        source = root / "source"
        source.mkdir()
        command("git", "init", "-q", source)
        inherited = root / "other.git"
        command("git", "init", "--bare", "-q", inherited)
        environment = dict(os.environ, GIT_DIR=str(inherited), GIT_WORK_TREE=str(inherited))
        result = command("git", "rev-parse", "--show-toplevel", cwd=source, env=environment)
        assert Path(result.stdout.decode().strip()) == source.resolve()


def test_command_runs_with_minimal_path():
    with tempfile.TemporaryDirectory() as temporary:
        environment = {"PATH": "/usr/bin:/bin"}
        result = command("sh", "-c", "printf ok", cwd=temporary, env=environment)
        assert result.stdout == b"ok"


if __name__ == "__main__":
    test_command_clears_inherited_git_repository_variables()
    test_command_runs_with_minimal_path()
    print("verifier runtime isolation: ok")
