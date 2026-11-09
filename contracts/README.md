# CLI contracts

The Backend publishes the authoritative snapshot in `backend/`, pinned by
`backend.lock.json`. Builds and CI use these versioned files without GitLab
credentials. `executor_sha` identifies the immutable CLI artifact;
`backend_sha` identifies the protected backend checkout used by the operation;
`revision` remains the requested application revision.

`backend/backup-manifest.schema.json` documents the backup archive manifest
validated by the restore path. Go tests validate serialized requests, results,
the actual binary identity, and backup manifests against these pinned schemas.

Updates require Python 3, PyYAML 6.0.3, and a private `GITLAB_TOKEN` in the
environment. Choose the Backend commit and its published manifest checksum:

```sh
python3 scripts/update-backend-contracts.py --backend-sha FULL_SHA --manifest-sha256 SHA256
python3 scripts/update-backend-contracts.py --check
```

The updater validates provenance, file checksums, and the OpenAPI structure
before replacing the snapshot. Review the snapshot and lock together in a PR.
