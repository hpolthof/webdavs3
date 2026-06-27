#!/usr/bin/env bash
# smoke_webui.sh — end-to-end web UI smoke test.
# Prerequisites:
#   - webdav3s binary running with admin + web UI on ADMIN_ENDPOINT.
#   - ADMIN_USER and ADMIN_PASS set to the admin UI credentials.
#   - curl and grep available.
#
# Usage:
#   ADMIN_USER=admin ADMIN_PASS=secret ./scripts/smoke_webui.sh

set -euo pipefail

ADMIN_ENDPOINT="${ADMIN_ENDPOINT:-http://localhost:9001}"
WEB_PASSWORD="${WEB_PASSWORD:-SmokeWebPass123}"
BUCKET="smoke-web-$(date +%s)"
COOKIE_JAR=$(mktemp)
trap 'rm -f "$COOKIE_JAR"' EXIT

export ADMIN_USER="${ADMIN_USER:?ADMIN_USER must be set}"
export ADMIN_PASS="${ADMIN_PASS:?ADMIN_PASS must be set}"

csrf_token() {
  local url=$1
  curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" "$url" | grep -oE 'name="csrf_token" value="[^"]+' | sed 's/.*"//' | tail -n1
}

echo "==> web UI smoke test against $ADMIN_ENDPOINT"

# 1. Admin login.
echo "--- 1. Admin login"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/admin/login")
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/admin/login" \
  -d "username=$ADMIN_USER" -d "password=$ADMIN_PASS" -d "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: admin login"; exit 1; }

# 2. Create a user and capture access key.
echo "--- 2. Create web UI user"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/admin/users/new")
CREATE_OUT=$(mktemp)
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/admin/users/new" \
  -d "display_name=SmokeUser" -d "web_password=$WEB_PASSWORD" -d "csrf_token=$TOKEN" > "$CREATE_OUT"
ACCESS_KEY=$(grep -oE 'Access Key</th><td><code>[^<]+' "$CREATE_OUT" | sed 's/.*<code>//' | tail -n1)
rm -f "$CREATE_OUT"
if [ -z "$ACCESS_KEY" ]; then
  echo "FAIL: could not extract access key"
  exit 1
fi
echo "    access key: $ACCESS_KEY"

# 3. Web UI login.
echo "--- 3. Web UI login"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/login")
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/login" \
  -d "access_key=$ACCESS_KEY" -d "password=$WEB_PASSWORD" -d "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: web UI login"; exit 1; }

# 4. Create bucket.
echo "--- 4. Create bucket: $BUCKET"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/buckets")
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/buckets" \
  -d "name=$BUCKET" -d "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: create bucket"; exit 1; }

# 5. Upload file.
echo "--- 5. Upload file via web UI"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/buckets/$BUCKET/browse")
TMPUP=$(mktemp /tmp/smoke-webui-XXXXXX.txt)
KEY=$(basename "$TMPUP")
echo -n "hello web ui" > "$TMPUP"
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/buckets/$BUCKET/upload" \
  -F "file=@$TMPUP" -F "prefix=" -F "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: upload"; exit 1; }
rm -f "$TMPUP"

# 6. Download file.
echo "--- 6. Download file via web UI"
TMPDOWN=$(mktemp)
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" "$ADMIN_ENDPOINT/buckets/$BUCKET/download?key=$KEY" > "$TMPDOWN"
CONTENT=$(cat "$TMPDOWN")
rm -f "$TMPDOWN"
if [ "$CONTENT" != "hello web ui" ]; then
  echo "FAIL: expected 'hello web ui', got '$CONTENT'"
  exit 1
fi
echo "    content verified: '$CONTENT'"

# 7. Delete object.
echo "--- 7. Delete object via web UI"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/buckets/$BUCKET/browse")
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/buckets/$BUCKET/objects/delete" \
  -d "key=$KEY" -d "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: delete object"; exit 1; }

# 8. Delete bucket.
echo "--- 8. Delete bucket via web UI"
TOKEN=$(csrf_token "$ADMIN_ENDPOINT/buckets")
curl -s -c "$COOKIE_JAR" -b "$COOKIE_JAR" -X POST "$ADMIN_ENDPOINT/buckets/$BUCKET/delete" \
  -d "csrf_token=$TOKEN" \
  -o /dev/null -w "%{http_code}\n" | grep -q "303\|302" || { echo "FAIL: delete bucket"; exit 1; }

echo "==> web UI smoke test PASSED"
