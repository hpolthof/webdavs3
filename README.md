# webdavs3

An S3-compatible storage gateway that uses WebDAV as its backend. Expose any WebDAV server as an S3 endpoint — compatible with `aws-cli`, `rclone`, `s3cmd`, and any other S3 client.

## How it works

- Clients talk S3 (AWS Signature V4) to webdavs3 on port 9000
- webdavs3 stores objects on WebDAV as SHA-256-hashed blobs under `/_data/`
- Large objects are automatically chunked into parts (configurable threshold) and stored under `/_parts/` with a manifest in the bucket metadata
- Metadata (users, buckets, objects) lives in SQLite files stored on the WebDAV backend itself, so a fresh daemon can recover by syncing
- A browser-based admin UI runs on port 9001 for managing WebDAV locations, users, and quotas

## Prerequisites

- Go 1.22+ (pure Go — no CGO required)
- A WebDAV server (Nextcloud, ownCloud, nginx with `dav_module`, Apache with `mod_dav`, etc.)

## Build

```bash
go build -o bin/webdavs3 ./cmd/webdavs3
# or
make build
```

## Configuration

Create `config.yaml`:

```yaml
# Listeners
s3_listen: ":9000"
admin_listen: ":9001"

# Admin UI credentials
admin_username: "admin"
admin_password_hash: "$2a$12$..."   # bcrypt hash of your admin password

# AES-256-GCM key for encrypting WebDAV passwords and S3 secrets at rest
# Generate: openssl rand -base64 32
encryption_key: "base64-encoded-32-byte-key=="

# Local directory for SQLite cache and daemon.id
local_cache_dir: "/var/cache/webdavs3"

# S3 region reported to clients
region: "us-east-1"

# How many bucket databases to keep open in memory (LRU)
bucket_cache_size: 50

# Chunking for large files (multipart upload)
chunk_threshold_mb: 500   # PutObject auto-chunks above this size (0 = disabled)
chunk_size_mb: 100        # size of each chunk

# How often to sync metadata from WebDAV
sync_interval: "5m"

# How often to flush usage stats to WebDAV
stats_flush_interval: "1m"

# Logging: format = "json" | "text", level = "debug" | "info" | "warn" | "error"
log_format: "json"
log_level: "info"
```

The `webdavs3 setup` command generates both values automatically. If you need to hash a password manually:

```bash
# htpasswd (Apache utils)
htpasswd -bnBC 12 "" yourpassword | tr -d ':\n'

# openssl (encryption key only)
openssl rand -base64 32
```

## First run

Run the setup wizard to create `config.yaml` (prompts for admin password, generates encryption key):

```bash
./bin/webdavs3 setup
# or specify a path:
./bin/webdavs3 setup /etc/webdavs3/config.yaml
```

The wizard writes a ready-to-use `config.yaml` with a bcrypt-hashed admin password and a randomly generated AES-256 encryption key. No external tools needed.

## Run

```bash
./bin/webdavs3 config.yaml
# or with defaults (looks for config.yaml in current directory)
./bin/webdavs3
```

On first start, the daemon generates a `daemon.id` file in `local_cache_dir` and creates the local SQLite databases. Add at least one WebDAV location via the admin UI before creating buckets.

## Startup provisioning

Set `WEBDAV3S_PROVISION_FILE` to a mounted YAML file when you want a brand-new instance to bootstrap WebDAV locations, S3 users, and buckets during daemon startup.

Example `provision.yaml`:

```yaml
users:
  - slug: backup-bot
    display_name: Backup Bot
    access_key: AKIAEXAMPLE
    secret_key: change-me
    web_password: change-me-too

locations:
  - slug: primary
    display_name: Primary Storage
    url: https://nextcloud.example.com/remote.php/dav/files/username/s3data
    username: webdav-user
    password: webdav-password
    quota_gb: 50
    base_dir: /
    buckets:
      - name: backups
        owner: backup-bot
```

Provisioning is a one-time bootstrap only. On startup, `webdavs3` applies the file exactly once for a new instance; if any users, locations, buckets, or the local `provisioned.flag` marker already exist, startup fails instead of reapplying the file.

After the first successful bootstrap, remove `WEBDAV3S_PROVISION_FILE` or leave the mounted file in place only if you expect startup to fail on reused state.

## First-time setup

1. **Open the admin UI** at `http://localhost:9001/admin/`
2. **Add a WebDAV location** at `/admin/locations/new`:
   - URL: e.g. `https://nextcloud.example.com/remote.php/dav/files/username/s3data`
   - Username and password for the WebDAV server
   - Quota in GB (0 = unlimited)
3. **Create an S3 user** at `/admin/users/new`:
   - Enter a display name
   - The access key and secret key are shown **once** — save them now
4. **Configure your S3 client** (see below)

## S3 client configuration

### aws-cli

```bash
aws configure
# AWS Access Key ID: <from admin UI>
# AWS Secret Access Key: <from admin UI>
# Default region: us-east-1
# Default output format: json

# Use --endpoint-url for every command:
aws s3api --endpoint-url http://localhost:9000 list-buckets
aws s3api --endpoint-url http://localhost:9000 create-bucket --bucket mybucket
aws s3 --endpoint-url http://localhost:9000 cp file.txt s3://mybucket/file.txt
aws s3 --endpoint-url http://localhost:9000 ls s3://mybucket/
```

Or set the endpoint permanently in `~/.aws/config`:

```ini
[default]
endpoint_url = http://localhost:9000
```

### rclone

```ini
# ~/.config/rclone/rclone.conf
[webdavs3]
type = s3
provider = Other
access_key_id = YOUR_ACCESS_KEY
secret_access_key = YOUR_SECRET_KEY
endpoint = http://localhost:9000
region = us-east-1
```

```bash
rclone ls webdavs3:mybucket
rclone copy localfile.txt webdavs3:mybucket/localfile.txt
```

### s3cmd

```bash
s3cmd --configure
# or directly:
s3cmd --access_key=YOUR_ACCESS_KEY \
      --secret_key=YOUR_SECRET_KEY \
      --host=localhost:9000 \
      --host-bucket="localhost:9000/%(bucket)s" \
      --no-ssl \
      ls s3://
```

## Supported S3 operations

| Operation | Description |
|-----------|-------------|
| `ListBuckets` | List all buckets for the authenticated user |
| `CreateBucket` | Create a new bucket (specify location via `x-amz-bucket-location` header, or uses first available) |
| `DeleteBucket` | Delete an empty bucket |
| `ListObjectsV2` | List objects with prefix, delimiter, and pagination |
| `PutObject` | Upload an object (auto-chunks above `chunk_threshold_mb`) |
| `GetObject` | Download an object (reassembles chunks transparently) |
| `DeleteObject` | Delete an object (removes chunks and manifest if chunked) |
| `HeadObject` | Get object metadata without downloading |
| `CreateMultipartUpload` | Start a multipart upload (for client-initiated chunking) |
| `UploadPart` | Upload a part (stored under `/_parts/{uploadID}/{partNum}`) |
| `CompleteMultipartUpload` | Assemble parts and create final object manifest |
| `AbortMultipartUpload` | Cancel and clean up a multipart upload |
| `ListMultipartUploads` | List in-progress multipart uploads for a bucket |
| `ListParts` | List uploaded parts for a multipart upload |

**Auto-chunking:** When `chunk_threshold_mb > 0`, `PutObject` automatically splits large uploads into chunks of size `chunk_size_mb`. This is transparent to the client and compatible with `aws-cli`, `rclone`, `s3cmd`, etc.

**S3 Multipart API:** The explicit multipart upload API (`CreateMultipartUpload` → `UploadPart` × N → `CompleteMultipartUpload`) is also supported for client-controlled chunking. Clients like AWS SDK can use either path.

**Not supported:** presigned URLs, object versioning, lifecycle policies, ACLs, server-side encryption (SSE).

## Storage layout on WebDAV

```
/_data/.tmp/               # temporary uploads (cleaned up after completion)
/_data/{xx}/{sha256hash}   # single-file object content (xx = first 2 hex chars of SHA-256)
/_parts/{upload-id}/{n}    # chunked object parts (kept after multipart completion)
/_meta/structure.db        # users, buckets, WebDAV locations
/_meta/{bucket-id}.db      # object index and chunk manifest for each bucket
/_meta/stats-{daemon-id}.db # usage statistics (one file per daemon instance)
```

Objects are stored by content hash (single files) or as a collection of numbered parts (chunked). The WebDAV contents are not directly usable without the metadata index — this prevents accidental access via the WebDAV interface.

Chunked objects have a manifest embedded in the bucket database tracking all parts, their ETags, and sizes. The `GetObject` handler reconstructs large objects by streaming all parts sequentially.

## Docker / container deployment

All config fields can be set via environment variables (prefix `WEBDAV3S_`). The config file is optional — the daemon starts with defaults if it does not exist.

| Environment variable | Config key | Default |
|---|---|---|
| `WEBDAV3S_S3_LISTEN` | `s3_listen` | `:9000` |
| `WEBDAV3S_ADMIN_LISTEN` | `admin_listen` | `:9001` |
| `WEBDAV3S_ADMIN_USERNAME` | `admin_username` | — |
| `WEBDAV3S_ADMIN_PASSWORD_HASH` | `admin_password_hash` | — |
| `WEBDAV3S_ENCRYPTION_KEY` | `encryption_key` | — |
| `WEBDAV3S_LOCAL_CACHE_DIR` | `local_cache_dir` | — |
| `WEBDAV3S_REGION` | `region` | `us-east-1` |
| `WEBDAV3S_PROVISION_FILE` | startup provisioning file | unset |
| `WEBDAV3S_BUCKET_CACHE_SIZE` | `bucket_cache_size` | `50` |
| `WEBDAV3S_CHUNK_THRESHOLD_MB` | `chunk_threshold_mb` | `0` (disabled) |
| `WEBDAV3S_CHUNK_SIZE_MB` | `chunk_size_mb` | `100` |
| `WEBDAV3S_SYNC_INTERVAL` | `sync_interval` | `5m` |
| `WEBDAV3S_STATS_FLUSH_INTERVAL` | `stats_flush_interval` | `1m` |
| `WEBDAV3S_LOG_FORMAT` | `log_format` | `json` |
| `WEBDAV3S_LOG_LEVEL` | `log_level` | `info` |
| `WEBDAV3S_DAEMON_ID` | `daemon_id` | auto-generated |

ENV vars override YAML values. Sensitive values (`WEBDAV3S_ENCRYPTION_KEY`, `WEBDAV3S_ADMIN_PASSWORD_HASH`) are well-suited for Docker secrets or a secrets manager.

Example `docker-compose.yml`:

```yaml
services:
  webdavs3:
    image: webdavs3:latest
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      WEBDAV3S_ENCRYPTION_KEY: "${ENCRYPTION_KEY}"
      WEBDAV3S_ADMIN_USERNAME: "admin"
      WEBDAV3S_ADMIN_PASSWORD_HASH: "${ADMIN_PASSWORD_HASH}"
      WEBDAV3S_LOCAL_CACHE_DIR: "/data/cache"
      WEBDAV3S_PROVISION_FILE: "/run/config/provision.yaml"
      WEBDAV3S_LOG_FORMAT: "json"
    volumes:
      - webdavs3-cache:/data/cache
      - ./provision.yaml:/run/config/provision.yaml:ro

volumes:
  webdavs3-cache:
```

Quick start with the published image:

```bash
docker pull ghcr.io/hpolthof/webdavs3:latest
docker run -d \
  --name webdavs3 \
  -p 9000:9000 \
  -p 9001:9001 \
  -e WEBDAV3S_ENCRYPTION_KEY="$ENCRYPTION_KEY" \
  -e WEBDAV3S_ADMIN_USERNAME="admin" \
  -e WEBDAV3S_ADMIN_PASSWORD_HASH="$ADMIN_PASSWORD_HASH" \
  -e WEBDAV3S_LOCAL_CACHE_DIR="/data/cache" \
  -e WEBDAV3S_PROVISION_FILE="/run/config/provision.yaml" \
  -v webdavs3-cache:/data/cache \
  -v "$(pwd)/provision.yaml:/run/config/provision.yaml:ro" \
  ghcr.io/hpolthof/webdavs3:latest
```

If you prefer Compose, keep the tag pinned to a release such as `ghcr.io/hpolthof/webdavs3:1.0.0` and only move it when you want to upgrade. Release tags publish semver image tags like `:1`, `:1.0`, and `:1.0.0`; `master` also publishes `:latest`, and every build gets a short `:sha-<commit>` tag for traceability.

For Docker, Coolify, and similar platforms, mount `provision.yaml` as a read-only file inside the container and point `WEBDAV3S_PROVISION_FILE` at that path. Use this only for first boot of a fresh `local_cache_dir`; once the instance has been provisioned, the next startup with the same cache directory will fail if the file is still enforced against existing state.

The runtime image is `scratch`, so there is no shell or package manager in the final container. Use `docker logs` for inspection, or build a temporary debug image from the same Dockerfile builder stage if you need an interactive troubleshooting shell.

For Coolify healthchecks, use the built-in command instead of `curl` or `wget`:

```text
Type: CMD
Command: /webdavs3 healthcheck
Interval: 10
Timeout: 5
Retries: 3
Start Period: 15
```

The command loads the same config and environment overrides as the daemon, then checks `http://127.0.0.1:<admin_port>/admin/login`. If you pass a config file to the daemon, pass the same file to the healthcheck, for example `/webdavs3 healthcheck /etc/webdavs3/config.yaml`.

Example build command:

```bash
docker build -t webdavs3:latest .
```

If you publish through GitHub Actions, the image name in GHCR will be `ghcr.io/hpolthof/webdavs3`. For example:

```bash
docker pull ghcr.io/hpolthof/webdavs3:latest
docker run --rm ghcr.io/hpolthof/webdavs3:latest --help
```

## Running multiple daemons (load sharing)

Multiple daemon instances can serve the same WebDAV backend:

- Each daemon writes to its own `stats-{daemon-id}.db` — no write conflicts on stats
- Bucket databases are per-bucket, so two daemons writing to different buckets have no conflicts
- Use a standard HTTP load balancer (nginx, HAProxy) in front of port 9000

Put `daemon_id` in each config or leave it empty to auto-generate. With auto-generated IDs, each instance maintains its own stats file.

## Sync

On startup, the daemon syncs metadata from the WebDAV backend. You can also trigger a manual sync per location from the admin UI (`/admin/locations/{id}` → Sync button).

The sync interval is configurable via `sync_interval` (default: `5m`).

## Admin UI reference

| Route | Description |
|-------|-------------|
| `/admin/` | Dashboard — usage per WebDAV location |
| `/admin/locations` | List WebDAV locations |
| `/admin/locations/new` | Add a WebDAV location |
| `/admin/locations/{id}` | Location detail and sync |
| `/admin/users` | List S3 users |
| `/admin/users/new` | Create an S3 user (access key + secret shown once) |
| `/admin/users/{id}` | User detail, enable/disable |

## Smoke test

With the daemon running and a user created:

```bash
TEST_ACCESS_KEY=mykey TEST_SECRET_KEY=mysecret ./scripts/smoke_test.sh
```

The script runs 15 operations (create bucket, put objects, list, get, delete) and exits non-zero on any failure.

## Export current provisioning state

Export the current locations, users, and buckets as provisioning YAML:

```bash
./bin/webdavs3 provision dump config.yaml > provision.yaml
```

The dump contains plaintext WebDAV passwords, S3 secret keys, and web UI passwords. Treat `provision.yaml` like a credential backup and store or transmit it accordingly.

## Development

```bash
# Run tests (requires Go 1.22+, no CGO)
go test ./...

# Run with text logging for readability
log_format: text
log_level: debug
```

## Known limitations

- No presigned URLs
- Object deduplication: two keys with identical content share one WebDAV file. Deleting one key does not remove the file (orphan cleanup is not yet implemented)
- WebDAV operations do not respect request context deadlines (cancellation is best-effort)
- Users created before enabling `encryption_key` will have invalid secrets and must be recreated

## License

MIT
