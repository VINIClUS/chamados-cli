#!/usr/bin/env python3
"""Check the structural contract files shipped by the standalone CLI."""

from __future__ import annotations

import json
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - CI installs PyYAML
    yaml = None


ROOT = Path(__file__).resolve().parents[2]


def load(name: str) -> dict:
    path = ROOT / "contracts" / name
    value = json.loads(path.read_text(encoding="utf-8"))
    assert value["$schema"].startswith("https://json-schema.org/"), name
    assert value["type"] == "object" and value["additionalProperties"] is False, name
    return value


def main() -> None:
    input_schema = load("operations-input.schema.json")
    result_schema = load("operations-result.schema.json")
    manifest_schema = load("executor-manifest.schema.json")
    backup_schema = load("backup-manifest.schema.json")
    assert set(input_schema["required"]) == {"operation", "environment", "ref", "tail"}
    assert set(result_schema["required"]) == {"schema_version", "operation", "environment", "status", "revision"}
    assert set(manifest_schema["required"]) == {"protocol", "executor_sha", "sha256", "os", "arch"}
    assert set(backup_schema["required"]) >= {"schema_version", "environment", "revision", "members"}
    backend = ROOT / "contracts" / "backend" / "openapi.yaml"
    if backend.exists():
        backend_directory = backend.parent
        for name in (
            "openapi.yaml",
            "responses.json",
            "operations-input.schema.json",
            "operations-result.schema.json",
            "executor-manifest.schema.json",
            "backup-manifest.schema.json",
        ):
            assert (backend_directory / name).is_file(), f"backend contract missing {name}"
        assert yaml is not None, "PyYAML is required to validate backend OpenAPI"
        document = yaml.safe_load(backend.read_text(encoding="utf-8"))
        assert isinstance(document, dict) and isinstance(document.get("openapi"), str)
        assert isinstance(document.get("info"), dict) and document["info"].get("title")
        assert isinstance(document.get("paths"), dict) and document["paths"]
        assert isinstance(document.get("components"), dict)
        schemas = document["components"].get("schemas", {})
        assert isinstance(schemas, dict), "backend OpenAPI schemas missing"
        for path, item in document["paths"].items():
            assert isinstance(path, str) and path.startswith("/")
            assert isinstance(item, dict), f"invalid path item: {path}"
            for operation in item.values():
                if not isinstance(operation, dict):
                    continue
                for response in operation.get("responses", {}).values():
                    assert isinstance(response, dict), f"invalid response: {path}"
                    for content in response.get("content", {}).values():
                        schema = content.get("schema", {})
                        if "$ref" in schema:
                            assert schema["$ref"].startswith("#/components/")
    lock = ROOT / "contracts" / "backend.lock.json"
    if lock.exists():
        metadata = json.loads(lock.read_text(encoding="utf-8"))
        assert metadata.get("schema_version") == 1
        assert metadata.get("project_id") and metadata.get("backend_sha")
        assert metadata.get("manifest_sha256") and metadata.get("files")
    print("contract structure: ok")


if __name__ == "__main__":
    main()
