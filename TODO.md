# TODO: metadata integrity and WebDAV inspection

## Current findings

- WebDAV can contain more physical data under `/_data/` and `/_parts/` than the Web UI shows, because the Web UI is driven by the local bucket metadata databases.
- If a `PutObject` uploads object bytes successfully but fails while writing or uploading bucket metadata, the remote object bytes can become unindexed/orphaned.
- A user should normally only receive success after metadata has been written and flushed, but older bugs, client disconnects, forced stops, or later metadata corruption can still leave remote bytes that are not visible locally.
- The log showed one repeatedly corrupt remote bucket metadata database:
  - `/dev/_meta/6daf88dd-0f59-47d5-a50c-a3cd9201c951.db`
  - SQLite `integrity_check` errors included missing rows and non-unique index entries.
- The app now refuses to overwrite local healthy metadata with corrupt remote metadata. That prevents local `malformed disk image`, but it also means the remote corrupt DB remains a problem until repaired.
- Stack/WebDAV also showed repeated transport errors:
  - `EOF`
  - `broken pipe`
  - `server closed idle connection`
  - `use of closed network connection`
  - failing `PUT`, `MKCOL`, and `PROPFIND/Stat` calls.
- A healthy-looking local main DB can still fail during repair export if SQLite
  checkpointing hits a malformed WAL/sidecar state.
- During bulk uploads, periodic sync can see a valid but stale remote bucket DB
  while metadata uploads are retrying. That remote DB must not overwrite a
  healthy local bucket DB.

## Fixes already implemented

- Remote bucket DB sync now downloads to a temp file, validates SQLite integrity, and only then replaces the local DB.
- Graceful shutdown now flushes `structure.db` and bucket metadata, not just stats.
- Metadata DB uploads now use temp-upload plus verify plus WebDAV rename instead of direct overwrite of the final `.db` path.
- Remote repair now falls back to a read-only immutable export if normal bucket DB export fails during checkpoint, and backs up local sidecar files when present.
- Periodic sync now keeps healthy local bucket metadata instead of replacing it
  with a stale-but-valid remote DB.
- These changes reduce the chance of local corruption and stale-size verification failures.

## Inspect command to add

- [x] Implement read-only inspect CLI command.

Add a read-only CLI command:

```bash
webdavs3 inspect [--json] [--bucket <name>] [config.yaml]
```

The command must never delete, rewrite, repair, or rename remote data. It only reports.

### Inspect command responsibilities

- [x] Open local `structure.db`.
- [x] For each bucket, open local `bucket-<id>.db`.
- [x] Validate every local bucket DB with SQLite `PRAGMA integrity_check`.
- [x] Read referenced paths from local metadata:
  - regular object `hash_path`
  - chunked object `object_chunks.path`
  - multipart/in-progress part paths where useful
- [x] List remote WebDAV paths under:
  - `{base_dir}_data/`
  - `{base_dir}_parts/`
  - `{base_dir}_meta/`
- [x] Compare remote object/chunk paths to local metadata references.
- [x] Report:
  - total remote data files and bytes
  - total locally referenced files and bytes
  - remote orphan/unreferenced files and bytes
  - missing remote files referenced by local metadata
  - remote bucket DB integrity status
  - local bucket DB integrity status
  - stats DB and structure DB presence

### Suggested output

Text output should be human-readable by default:

```text
Location: Stack Test (/dev/)
Bucket: testing (6daf88dd-...)
  local metadata: ok
  remote metadata: corrupt (row 1 missing from index sqlite_autoindex_objects_2)
  referenced data: 1234 files, 18.4 GiB
  remote data:     1391 files, 21.7 GiB
  orphan data:      157 files, 3.3 GiB
  missing data:       0 files
```

`--json` should emit machine-readable data for support/debugging.

## Repair gate to add

- [x] Implement per-bucket metadata health state.

Add per-bucket metadata health:

- `healthy`: writes allowed.
- `repairing`: writes return temporary retryable failure.
- `degraded`: writes remain blocked until repair succeeds or operator intervenes.

When remote metadata is corrupt but local metadata is healthy:

1. [x] Mark bucket `repairing`.
2. [x] Block new writes for that bucket before uploading object bytes.
3. [x] Validate local DB.
4. [x] Export local DB with `VACUUM INTO`.
5. [x] Upload to WebDAV temp path.
6. [x] Verify temp path size.
7. [x] Rename over final remote DB.
8. [x] Download final remote DB and run `integrity_check`.
9. [x] Mark bucket `healthy`.

- [x] If local metadata is missing or corrupt, do not repair automatically. Mark `degraded` and require manual restore.

## Bulk upload behavior target

- [x] Bulk uploads should not create hundreds of repeated metadata failures.
- [x] If metadata is healthy, uploads continue normally.
- [x] If metadata becomes corrupt or unavailable, the bucket should pause writes quickly.
- [x] Clients should receive a retryable S3 error, preferably `503 ServiceUnavailable` or `SlowDown` with retry guidance.
- [ ] A single bucket-level repair worker should attempt repair, instead of every `PutObject` trying the same repair independently.

## Data preservation rules

- [x] Never overwrite local metadata with remote metadata unless remote SQLite integrity passes.
- [x] Never overwrite remote metadata from local metadata unless local SQLite integrity passes.
- [x] Never run GC while metadata integrity is uncertain.
- [x] Always create a local backup before repair:
  - `bucket-<id>.db.before-repair`
  - `bucket-<id>.db-wal.before-repair` when present
  - `bucket-<id>.db-shm.before-repair` when present
- [x] Object bytes under `/_data/` and `/_parts/` are preserved during inspect and repair.
- [ ] Cleanup of orphan data should be a separate explicit command after an inspect report has been reviewed.

## Release / upgrade behavior

- [x] On startup or first sync after upgrade, detect corrupt remote bucket DBs.
- [x] If local bucket DB is healthy, repair remote metadata from local before allowing writes.
- [x] If local bucket DB is missing or corrupt, mark bucket degraded and require manual restore.
- [x] Log clear operator guidance for the affected bucket ID and remote path.

## Later cleanup command

- [ ] Add guarded cleanup command after inspect exists and has been verified.

Only after inspect exists and has been verified, add a separate guarded cleanup command:

```bash
webdavs3 gc --dry-run [config.yaml]
webdavs3 gc --force [config.yaml]
```

- [ ] Ensure cleanup deletes only remote data that is unreferenced by healthy local metadata.
- [ ] Ensure cleanup never runs automatically.
