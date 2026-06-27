#!/usr/bin/env bash
# smoke_test.sh — end-to-end S3 API smoke test using the aws CLI.
# Prerequisites:
#   - webdavs3 binary running at ENDPOINT with test credentials in env.
#   - AWS CLI installed and in PATH.
#   - TEST_ACCESS_KEY and TEST_SECRET_KEY set in environment.
#
# Usage:
#   TEST_ACCESS_KEY=mykey TEST_SECRET_KEY=mysecret ./scripts/smoke_test.sh

set -euo pipefail

ENDPOINT="${ENDPOINT:-http://localhost:9000}"
REGION="${REGION:-us-east-1}"
BUCKET="smoke-test-$(date +%s)"

AWS="aws s3api \
  --endpoint-url $ENDPOINT \
  --region $REGION \
  --no-verify-ssl"

export AWS_ACCESS_KEY_ID="${TEST_ACCESS_KEY:?TEST_ACCESS_KEY must be set}"
export AWS_SECRET_ACCESS_KEY="${TEST_SECRET_KEY:?TEST_SECRET_KEY must be set}"
export AWS_DEFAULT_REGION="$REGION"

echo "==> smoke test against $ENDPOINT"

# 1. List buckets (expect empty or existing list — just check it succeeds).
echo "--- 1. ListBuckets"
$AWS list-buckets

# 2. Create bucket.
echo "--- 2. CreateBucket: $BUCKET"
$AWS create-bucket --bucket "$BUCKET"

# 3. Verify bucket appears in list.
echo "--- 3. ListBuckets after create"
$AWS list-buckets | grep "$BUCKET"

# 4. Put object.
echo "--- 4. PutObject: hello.txt"
echo -n "hello world" | $AWS put-object \
  --bucket "$BUCKET" \
  --key "hello.txt" \
  --body /dev/stdin \
  --content-type "text/plain"

# 5. Get object and verify content.
echo "--- 5. GetObject: hello.txt"
TMPFILE=$(mktemp)
$AWS get-object --bucket "$BUCKET" --key "hello.txt" "$TMPFILE"
CONTENT=$(cat "$TMPFILE")
rm -f "$TMPFILE"
if [ "$CONTENT" != "hello world" ]; then
  echo "FAIL: expected 'hello world', got '$CONTENT'"
  exit 1
fi
echo "    content verified: '$CONTENT'"

# 6. Head object.
echo "--- 6. HeadObject: hello.txt"
$AWS head-object --bucket "$BUCKET" --key "hello.txt"

# 7. List objects in bucket.
echo "--- 7. ListObjectsV2"
$AWS list-objects-v2 --bucket "$BUCKET" | grep "hello.txt"

# 8. Put a second object.
echo "--- 8. PutObject: dir/world.txt"
echo -n "world" | $AWS put-object \
  --bucket "$BUCKET" \
  --key "dir/world.txt" \
  --body /dev/stdin \
  --content-type "text/plain"

# 9. List with prefix.
echo "--- 9. ListObjectsV2 with prefix=dir/"
RESULT=$($AWS list-objects-v2 --bucket "$BUCKET" --prefix "dir/")
echo "$RESULT" | grep "dir/world.txt"

# 10. List with delimiter (common prefixes).
echo "--- 10. ListObjectsV2 with delimiter=/"
$AWS list-objects-v2 --bucket "$BUCKET" --delimiter "/"

# 11. Delete first object.
echo "--- 11. DeleteObject: hello.txt"
$AWS delete-object --bucket "$BUCKET" --key "hello.txt"

# 12. Confirm deletion.
echo "--- 12. HeadObject after delete (expect 404)"
if $AWS head-object --bucket "$BUCKET" --key "hello.txt" 2>/dev/null; then
  echo "FAIL: object should have been deleted"
  exit 1
fi
echo "    confirmed deleted"

# 13. Delete second object.
echo "--- 13. DeleteObject: dir/world.txt"
$AWS delete-object --bucket "$BUCKET" --key "dir/world.txt"

# 14. Delete bucket.
echo "--- 14. DeleteBucket: $BUCKET"
$AWS delete-bucket --bucket "$BUCKET"

# 15. Confirm bucket is gone.
echo "--- 15. ListBuckets after delete"
BUCKETS=$($AWS list-buckets)
if echo "$BUCKETS" | grep -q "$BUCKET"; then
  echo "FAIL: bucket should have been deleted"
  exit 1
fi
echo "    confirmed bucket deleted"

echo "==> smoke test PASSED"
