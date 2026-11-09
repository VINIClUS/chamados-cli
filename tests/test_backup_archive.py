"""Archive contract: real tar files; corrupt input must never reach extraction."""

import hashlib
import io
import json
import subprocess
import tarfile
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts/operations/backup_archive.py"


def test_source_extract_keeps_application_and_excludes_operational_directories(tmp_path):
    source = tmp_path / "source.tar.gz"
    with tarfile.open(source, "w:gz") as archive:
        for name in (
            "application.txt",
            "infrastructure/application/backend.Dockerfile",
            "devops/tooling.txt",
            "cicd/pipeline.yml",
            ".gitlab/pipeline.yml",
            ".gitlab-ci.yml",
            "infrastructure/environments/docker-compose.production.yml",
            "infrastructure/nginx/nginx-production.conf",
            "infrastructure/scripts/bootstrap-host.sh",
            "infrastructure/legacy/java17.Dockerfile",
        ):
            member = tarfile.TarInfo("project/" + name)
            member.mode = 0o600
            member.size = 4
            archive.addfile(member, io.BytesIO(b"data"))
    destination = tmp_path / "application"
    result = run("source-extract", source, destination)
    assert result.returncode == 0, result.stderr
    assert (destination / "application.txt").read_bytes() == b"data"
    assert (destination / "infrastructure/application/backend.Dockerfile").read_bytes() == b"data"
    assert not (destination / "devops").exists()
    assert not (destination / "cicd").exists()
    assert not (destination / ".gitlab").exists()
    assert not (destination / ".gitlab-ci.yml").exists()
    for protected in ("environments", "nginx", "scripts", "legacy"):
        assert not (destination / "infrastructure" / protected).exists()


def run(*args):
    return subprocess.run(["python3", str(HELPER), *map(str, args)], capture_output=True, text=True)


def snapshot(tmp_path):
    data = tmp_path / "data"
    data.mkdir()
    (data / "database.dump").write_bytes(b"PGDMP\x01snapshot")
    with tarfile.open(data / "attachments.tar", "w") as tar:
        info = tarfile.TarInfo("folder/anexo.txt")
        info.size = 6
        tar.addfile(info, io.BytesIO(b"anexo!"))
    meta = tmp_path / "metadata.json"
    meta.write_text(
        json.dumps(
            dict(
                schema_version=1,
                environment="staging",
                revision="a" * 40,
                ref="main",
                postgres_major=16,
                created_at="2026-11-07T12:00:00Z",
            )
        )
    )
    output = tmp_path / "output"
    result = run("pack", data, meta, output)
    assert result.returncode == 0, result.stderr
    return output


def test_roundtrip_verifies_content_and_excludes_env(tmp_path):
    output = snapshot(tmp_path)
    manifest = json.loads((output / "manifest.json").read_text())
    assert manifest["sha256"] == hashlib.sha256((output / "backup.tar.gz").read_bytes()).hexdigest()
    assert {m["name"] for m in manifest["members"]} == {"database.dump", "attachments.tar"}
    dest = tmp_path / "validated"
    result = run("verify", output / "backup.tar.gz", output / "manifest.json", "staging", "16", dest)
    assert result.returncode == 0, result.stderr
    assert (dest / "database.dump").read_bytes() == b"PGDMP\x01snapshot"
    assert not (dest / ".env").exists()


@pytest.mark.parametrize(
    "field,value",
    [("environment", "production"), ("postgres_major", 17), ("sha256", "0" * 64), ("revision", "short-dirty")],
)
def test_rejects_incompatible_or_corrupt_manifest_before_extracting(tmp_path, field, value):
    output = snapshot(tmp_path)
    manifest = json.loads((output / "manifest.json").read_text())
    manifest[field] = value
    (output / "manifest.json").write_text(json.dumps(manifest))
    dest = tmp_path / "validated"
    result = run("verify", output / "backup.tar.gz", output / "manifest.json", "staging", "16", dest)
    assert result.returncode != 0
    assert not dest.exists()


@pytest.mark.parametrize("name,link", [("../../escape", False), ("/tmp/escape", False), ("symlink", True)])
def test_rejects_unsafe_nested_attachment_tar(tmp_path, name, link):
    data = tmp_path / "data"
    data.mkdir()
    (data / "database.dump").write_bytes(b"PGDMPsnapshot")
    with tarfile.open(data / "attachments.tar", "w") as tar:
        info = tarfile.TarInfo(name)
        if link:
            info.type = tarfile.SYMTYPE
            info.linkname = "/etc/passwd"
        tar.addfile(info)
    meta = tmp_path / "meta.json"
    meta.write_text(
        json.dumps(
            dict(
                schema_version=1,
                environment="staging",
                revision="a" * 40,
                ref="main",
                postgres_major=16,
                created_at="2026-11-07T12:00:00Z",
            )
        )
    )
    result = run("pack", data, meta, tmp_path / "out")
    assert result.returncode != 0


def test_rejects_unlisted_outer_archive_member_even_with_valid_archive_hash(tmp_path):
    output = snapshot(tmp_path)
    original = output / "backup.tar.gz"
    altered = tmp_path / "altered.tar.gz"
    with tarfile.open(original) as src, tarfile.open(altered, "w:gz") as dst:
        for member in src.getmembers():
            dst.addfile(member, src.extractfile(member))
        dst.addfile(tarfile.TarInfo(".env"))
    manifest = json.loads((output / "manifest.json").read_text())
    manifest["sha256"] = hashlib.sha256(altered.read_bytes()).hexdigest()
    (output / "manifest.json").write_text(json.dumps(manifest))
    dest = tmp_path / "validated"
    assert run("verify", altered, output / "manifest.json", "staging", "16", dest).returncode != 0
    assert not dest.exists()


def test_attachment_directories_and_binary_files_roundtrip_with_exact_contents(tmp_path):
    volume = tmp_path / "volume"
    (volume / "one/two").mkdir(parents=True)
    (volume / "one/two/file.bin").write_bytes(b"\x00\xffattachment")
    archive = tmp_path / "attachments.tar"
    assert run("attachments", volume, archive).returncode == 0
    (volume / "stale").write_text("must disappear")
    result = run("extract-attachments", archive, volume)
    assert result.returncode == 0, result.stderr
    assert (volume / "one/two/file.bin").read_bytes() == b"\x00\xffattachment"
    assert not (volume / "stale").exists()


def test_tar_rejects_file_ancestor_even_when_it_appears_after_child(tmp_path):
    data = tmp_path / "data"
    data.mkdir()
    (data / "database.dump").write_bytes(b"PGDMPsnapshot")
    with tarfile.open(data / "attachments.tar", "w") as archive:
        archive.addfile(tarfile.TarInfo("parent/child"))
        archive.addfile(tarfile.TarInfo("parent"))
    meta = tmp_path / "meta.json"
    meta.write_text(
        json.dumps(
            dict(
                schema_version=1,
                environment="staging",
                revision="a" * 40,
                ref="main",
                postgres_major=16,
                created_at="2026-11-07T12:00:00Z",
            )
        )
    )
    assert run("pack", data, meta, tmp_path / "out").returncode != 0
