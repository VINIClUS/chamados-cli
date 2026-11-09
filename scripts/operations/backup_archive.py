#!/usr/bin/env python3
"""Strict, portable backup format. Uses only Python's standard library on CI/CT.

No extraction occurs until both manifests, every checksum, and both tar member
sets have been checked. Links/devices/FIFOs and ambiguous paths are forbidden.
"""

import contextlib
import datetime as dt
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path, PurePosixPath


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        result = super().redirect_request(req, fp, code, msg, headers, newurl)
        if result is None:
            return None
        if urllib.parse.urlsplit(req.full_url)[:2] != urllib.parse.urlsplit(newurl)[:2]:
            if urllib.parse.urlsplit(newurl).scheme != "https":
                raise ValueError("refusing insecure registry redirect")
            result.remove_header("Job-token")
        return result


def archive_file(archive: tarfile.TarFile, member: tarfile.TarInfo):
    stream = archive.extractfile(member)
    if stream is None:
        raise ValueError("archive member is not a regular file: " + member.name)
    return stream


def api(path, *, destination=None, upload=None, method="GET", missing=False):
    root = os.environ.get("CI_API_V4_URL", "https://gitlab.com/api/v4").rstrip("/")
    project = os.environ.get("CI_PROJECT_ID", "74155066")
    if not re.fullmatch(r"[0-9]+", project):
        raise ValueError("invalid project ID")
    token = os.environ["CI_JOB_TOKEN"]
    request = urllib.request.Request(
        root + "/projects/" + project + "/" + path, method=method, headers={"JOB-TOKEN": token}
    )
    if upload:
        request.add_header("Content-Type", "application/octet-stream")
        request.add_header("Content-Length", str(Path(upload).stat().st_size))
    try:
        with contextlib.ExitStack() as stack:
            if upload:
                request.data = stack.enter_context(open(upload, "rb"))
                request.add_header("Content-Length", str(Path(upload).stat().st_size))
            response = stack.enter_context(urllib.request.build_opener(SafeRedirect()).open(request, timeout=120))
            if destination:
                with open(destination, "wb") as output:
                    shutil.copyfileobj(response, output)
                return None
            body = response.read()
            return json.loads(body) if body else None
    except urllib.error.HTTPError as exc:
        if missing and exc.code == 404:
            return None
        # Never include the token, request headers or server response body.
        raise ValueError("GitLab API HTTP %s for %s" % (exc.code, path)) from None


def package_path(version, filename):
    if not re.fullmatch(r"[0-9]{14}\.[0-9]+", version):
        raise ValueError("invalid backup_id")
    environment = os.environ["OPS_ENVIRONMENT"]
    if environment not in ("staging", "production"):
        raise ValueError("invalid registry environment")
    return "packages/generic/chamados-backup-%s/%s/%s" % (environment, version, filename)


def publish(directory, version):
    root = Path(directory)
    manifest = metadata(json.loads((root / "manifest.json").read_text()))
    if manifest["environment"] != os.environ["OPS_ENVIRONMENT"]:
        raise ValueError("cannot publish backup to another environment")
    if manifest["sha256"] != digest(root / "backup.tar.gz"):
        raise ValueError("refusing to publish corrupt backup")
    if api(package_path(version, "manifest.json"), missing=True) is not None:
        raise ValueError("backup_id already committed; refusing overwrite")
    api(package_path(version, "backup.tar.gz"), upload=root / "backup.tar.gz", method="PUT")
    # A package becomes visible to readers only after this final commit marker.
    api(package_path(version, "manifest.json"), upload=root / "manifest.json", method="PUT")


def download(version, directory):
    root = Path(directory)
    root.mkdir(mode=0o700)
    try:
        api(package_path(version, "manifest.json"), destination=root / "manifest.json")
        api(package_path(version, "backup.tar.gz"), destination=root / "backup.tar.gz")
    except Exception:
        shutil.rmtree(root)
        raise


def verify_production(revision, ref):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", ref) or not re.fullmatch(r"[a-f0-9]{40}", revision):
        raise ValueError("production requires an immutable vX.Y.Z tag and full revision")
    tag = api("repository/tags/" + urllib.parse.quote(ref, safe=""))
    if tag.get("protected") is not True or tag.get("commit", {}).get("id") != revision:
        raise ValueError("production tag is unprotected or references another revision")
    # CI_JOB_TOKEN permits Tags/Commits APIs and Git-over-HTTP, but GitLab's
    # merge_base REST endpoint does not accept it. Fetch refs with an ephemeral
    # credential helper: the token appears only in the helper's stdin protocol.
    root = os.environ.get("CI_API_V4_URL", "https://gitlab.com/api/v4")
    parsed = urllib.parse.urlsplit(root)
    prefix = parsed.path.removesuffix("/api/v4").rstrip("/")
    project_path = os.environ.get("CI_PROJECT_PATH") or "portosoftware/SistemaDeChamados"
    if not re.fullmatch(r"[A-Za-z0-9_./-]+", project_path) or any(
        part in ("", ".", "..") for part in project_path.split("/")
    ):
        raise ValueError("invalid CI project path")
    repository = urllib.parse.urlunsplit(
        (parsed.scheme, parsed.netloc, prefix + "/" + urllib.parse.quote(project_path, safe="/") + ".git", "", "")
    )
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update(GIT_TERMINAL_PROMPT="0", GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
    credential = '!f() { printf "%s\\n" username=gitlab-ci-token; printf "password=%s\\n" "$CI_JOB_TOKEN"; }; f'
    with tempfile.TemporaryDirectory() as temporary:

        def git(*args):
            return subprocess.run(
                ["git", "-C", temporary, "-c", "credential.helper=", "-c", "credential.helper=" + credential, *args],
                env=env,
                capture_output=True,
                text=True,
                timeout=60,
            )

        if git("init", "--bare", "--quiet").returncode:
            raise ValueError("cannot initialize ancestry verification")
        if git(
            "fetch",
            "--quiet",
            "--no-tags",
            "--filter=blob:none",
            repository,
            "+refs/heads/main:refs/heads/main",
            "+refs/tags/" + ref + ":refs/tags/" + ref,
        ).returncode:
            raise ValueError("cannot fetch production tag and main with CI_JOB_TOKEN")
        if git("rev-parse", ref + "^{commit}").stdout.strip() != revision:
            raise ValueError("production tag changed during verification")
        if git("merge-base", "--is-ancestor", revision, "refs/heads/main").returncode:
            raise ValueError("production backup revision is not an ancestor of main")


def resolve_revision(revision):
    if not re.fullmatch(r"[a-f0-9]{7,40}", revision):
        raise ValueError("legacy dirty/unknown revision cannot be backed up")
    commit = api("repository/commits/" + revision)
    full = commit.get("id", "")
    if not re.fullmatch(r"[a-f0-9]{40}", full) or not full.startswith(revision):
        raise ValueError("cannot resolve deployed revision")
    print(full)


def source_download(revision, output):
    if not re.fullmatch(r"[a-f0-9]{40}", revision):
        raise ValueError("source must use full SHA")
    api("repository/archive.tar.gz?sha=" + revision, destination=output)


def cleanup():
    name = "chamados-backup-" + os.environ["OPS_ENVIRONMENT"]
    packages, page = [], 1
    while True:
        batch = api(
            "packages?"
            + urllib.parse.urlencode(
                dict(
                    package_type="generic",
                    package_name=name,
                    per_page=100,
                    page=page,
                    order_by="created_at",
                    sort="desc",
                )
            )
        )
        packages.extend(
            p
            for p in batch
            if p.get("name") == name
            and p.get("package_type") == "generic"
            and re.fullmatch(r"[0-9]{14}\.[0-9]+", p.get("version", ""))
        )
        if len(batch) < 100:
            break
        page += 1
    keep = None
    for package in packages:
        try:
            with tempfile.TemporaryDirectory() as temporary:
                folder = Path(temporary) / "package"
                download(package["version"], folder)
                value = metadata(json.loads((folder / "manifest.json").read_text()))
                verify(
                    folder / "backup.tar.gz",
                    folder / "manifest.json",
                    os.environ["OPS_ENVIRONMENT"],
                    value["postgres_major"],
                    Path(temporary) / "verified",
                )
            keep = package["id"]
            break
        except (ValueError, OSError, urllib.error.URLError, tarfile.TarError):
            continue
    if keep is None:
        if packages:
            raise ValueError("cleanup refused: no complete valid backup found")
        return
    cutoff = dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=30)
    for package in packages:
        created = dt.datetime.fromisoformat(package["created_at"].replace("Z", "+00:00"))
        if package["id"] != keep and created < cutoff:
            api("packages/" + str(int(package["id"])), method="DELETE")


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def metadata(value):
    if value.get("schema_version") != 1 or value.get("environment") not in ("staging", "production"):
        raise ValueError("unsupported backup schema/environment")
    if not re.fullmatch(r"[0-9a-f]{40}", value.get("revision", "")):
        raise ValueError("backup revision must be a full immutable SHA")
    if not isinstance(value.get("postgres_major"), int) or value["postgres_major"] < 10:
        raise ValueError("invalid postgres_major")
    if not isinstance(value.get("ref"), str) or not value["ref"] or "\n" in value["ref"]:
        raise ValueError("missing source ref")
    dt.datetime.strptime(value["created_at"], "%Y-%m-%dT%H:%M:%SZ")
    return value


def safe_members(archive):
    seen = {}
    for member in archive.getmembers():
        name = member.name.removesuffix("/")
        # GNU tar commonly uses './'; allow it only as a directory prefix.
        while name.startswith("./"):
            name = name[2:]
        if name in ("", ".") and member.isdir():
            continue
        parts = PurePosixPath(name).parts
        if (
            not name
            or name.startswith("/")
            or ".." in parts
            or "\\" in name
            or any(p in ("", ".") for p in name.split("/"))
            or name in seen
            or not (member.isfile() or member.isdir())
        ):
            raise ValueError("unsafe or duplicate tar member: " + member.name)
        # A regular file may not be an ancestor of another entry.
        if any(
            parent in seen and seen[parent] == "file"
            for parent in (str(p) for p in PurePosixPath(name).parents if str(p) != ".")
        ):
            raise ValueError("file used as directory")
        seen[name] = "file" if member.isfile() else "directory"
    for name, kind in seen.items():
        if kind == "file" and any(other.startswith(name + "/") for other in seen):
            raise ValueError("file used as directory")
    return archive.getmembers()


def check_attachments(path):
    with tarfile.open(path, "r:") as archive:
        safe_members(archive)


def pack(data, meta, output):
    data, output = Path(data), Path(output)
    value = metadata(json.loads(Path(meta).read_text()))
    with (data / "database.dump").open("rb") as dump:
        magic = dump.read(5)
    if magic != b"PGDMP":
        raise ValueError("database.dump must use pg_dump custom format")
    check_attachments(data / "attachments.tar")
    value["members"] = [
        {"name": name, "sha256": digest(data / name), "size": (data / name).stat().st_size}
        for name in ("database.dump", "attachments.tar")
    ]
    output.mkdir(mode=0o700)
    inner = output / "manifest.json"
    inner.write_text(json.dumps(value, sort_keys=True) + "\n")
    with tarfile.open(output / "backup.tar.gz", "w:gz") as archive:
        for name in ("database.dump", "attachments.tar"):
            archive.add(data / name, arcname=name, recursive=False)
        archive.add(inner, arcname="manifest.json", recursive=False)
    value["sha256"] = digest(output / "backup.tar.gz")
    inner.write_text(json.dumps(value, sort_keys=True) + "\n")


def verify(archive_path, manifest_path, environment, major, destination):
    outer = metadata(json.loads(Path(manifest_path).read_text()))
    if outer["environment"] != environment or outer["postgres_major"] != int(major):
        raise ValueError("backup environment or PostgreSQL major is incompatible")
    if outer.get("sha256") != digest(archive_path):
        raise ValueError("backup archive checksum mismatch")
    with tempfile.TemporaryDirectory() as temporary, tarfile.open(archive_path, "r:gz") as archive:
        members = safe_members(archive)
        if {m.name for m in members} != {"database.dump", "attachments.tar", "manifest.json"} or any(
            not m.isfile() for m in members
        ):
            raise ValueError("unexpected backup members")
        inner_member = archive.getmember("manifest.json")
        if inner_member.size > 1024 * 1024:
            raise ValueError("oversized inner manifest")
        with archive_file(archive, inner_member) as stream:
            inner = json.load(stream)
        expected = {k: v for k, v in outer.items() if k != "sha256"}
        if inner != expected:
            raise ValueError("inner and external manifests differ")
        entries = inner.get("members", [])
        if len(entries) != 2 or {x["name"] for x in entries} != {"database.dump", "attachments.tar"}:
            raise ValueError("invalid checksummed member set")
        for entry in entries:
            member = archive.getmember(entry["name"])
            target = Path(temporary) / entry["name"]
            with archive_file(archive, member) as src, target.open("wb") as dst:
                shutil.copyfileobj(src, dst)
            if member.size != entry["size"] or digest(target) != entry["sha256"]:
                raise ValueError("member checksum mismatch: " + entry["name"])
        with (Path(temporary) / "database.dump").open("rb") as dump:
            magic = dump.read(5)
        if magic != b"PGDMP":
            raise ValueError("not a custom PostgreSQL dump")
        check_attachments(Path(temporary) / "attachments.tar")
        dest = Path(destination)
        if dest.exists():
            raise ValueError("validation destination already exists")
        dest.mkdir(mode=0o700)
        for entry in entries:
            shutil.copyfile(Path(temporary) / entry["name"], dest / entry["name"])
        (dest / "manifest.json").write_text(json.dumps(inner) + "\n")


def attachments(source, output):
    with tarfile.open(output, "w") as archive:
        for item in sorted(Path(source).rglob("*")):
            if item.is_symlink() or not (item.is_file() or item.is_dir()):
                raise ValueError("unsupported attachment type: " + str(item))
            archive.add(item, arcname=str(item.relative_to(source)), recursive=False)


def extract_attachments(source, destination):
    check_attachments(source)
    dest = Path(destination)
    if dest.is_symlink() or not dest.is_dir():
        raise ValueError("attachment destination must be an existing directory")
    # Only this explicitly selected Docker volume is emptied; .env is outside it.
    for entry in dest.iterdir():
        if entry.is_dir() and not entry.is_symlink():
            shutil.rmtree(entry)
        else:
            entry.unlink()
    with tarfile.open(source, "r:") as archive:
        for member in safe_members(archive):
            name = member.name
            while name.startswith("./"):
                name = name[2:]
            if not name or name == ".":
                continue
            target = dest / name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive_file(archive, member) as src, target.open("wb") as dst:
                    shutil.copyfileobj(src, dst)
                target.chmod(member.mode & 0o777)
                if os.geteuid() == 0:
                    os.chown(target, member.uid, member.gid)


def source_extract(source, destination):
    """Extract a GitLab source archive, stripping its single generated root."""
    dest = Path(destination)
    with tarfile.open(source, "r:gz") as archive:
        members = safe_members(archive)
        roots = {PurePosixPath(m.name).parts[0] for m in members}
        if len(roots) != 1:
            raise ValueError("source must have exactly one root")
        dest.mkdir(mode=0o700)
        for member in members:
            parts = PurePosixPath(member.name).parts[1:]
            if not parts:
                continue
            if ".env" in parts:
                raise ValueError("source archive contains .env")
            if parts[0] in {"devops", "cicd", ".gitlab", ".gitlab-ci.yml"}:
                continue
            if parts[0] == "scripts" and len(parts) > 1 and parts[1] in {"deploy", "operations"}:
                continue
            if parts[0] == "infrastructure" and len(parts) > 1 and parts[1] != "application":
                continue
            target = dest.joinpath(*parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive_file(archive, member) as src, target.open("wb") as dst:
                    shutil.copyfileobj(src, dst)
                target.chmod(member.mode & 0o777)


def main():
    commands = {
        "pack": pack,
        "verify": verify,
        "attachments": attachments,
        "extract-attachments": extract_attachments,
        "source-extract": source_extract,
        "publish": publish,
        "download": download,
        "verify-production": verify_production,
        "resolve-revision": resolve_revision,
        "source-download": source_download,
        "cleanup": cleanup,
    }
    commands[sys.argv[1]](*sys.argv[2:])


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, tarfile.TarError) as exc:
        print("backup: " + str(exc), file=sys.stderr)
        sys.exit(1)
