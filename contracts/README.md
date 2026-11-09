# CLI contracts

The operation input/result schemas and executor artifact manifest are versioned
with the standalone CLI. `executor_sha` identifies the immutable CLI artifact;
`backend_sha` identifies the protected backend checkout used by the operation;
`revision` remains the requested application revision.

`backup-manifest.schema.json` documents the backup archive manifest validated by
the restore path. It is kept alongside the three runner wire contracts while
the backend package snapshot remains the synchronization authority.

The backend OpenAPI snapshot is synchronized separately by the backend project
under `contracts/backend/` and is not copied into this repository.
