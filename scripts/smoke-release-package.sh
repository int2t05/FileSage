#!/usr/bin/env bash
set -euo pipefail

# 解包并以空 PATH 验证 Linux 发布包的基本服务流程。
if [ "$#" -ne 2 ]; then
  echo "usage: smoke-release-package.sh dist/FILE.tar.gz filesage_vX.Y.Z_linux_amd64" >&2
  exit 2
fi

archive="$1"
bundle="$2"
if [[ ! "$bundle" =~ ^filesage_v[0-9]+\.[0-9]+\.[0-9]+_linux_amd64$ ]]; then
  echo "invalid package name" >&2
  exit 2
fi
if [ "$archive" != "dist/$bundle.tar.gz" ] || [ ! -f "$archive" ]; then
  echo "release archive not found at expected path" >&2
  exit 2
fi

smoke="$(mktemp -d)"
server_pid=
cleanup() {
  result=$?
  trap - EXIT
  if [ -n "$server_pid" ]; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [ "$result" -ne 0 ] && [ -f "$smoke/server.log" ]; then
    cat "$smoke/server.log" >&2
  fi
  rm -rf -- "$smoke"
  exit "$result"
}
trap cleanup EXIT

mkdir -p "$smoke/extract" "$smoke/root" "$smoke/data" "$smoke/empty-path"
tar -xzf "$archive" -C "$smoke/extract"
package="$smoke/extract/$bundle"
test -x "$package/filesage"
test -s "$package/web/index.html"

port=18089
name="release-smoke-$$"
PATH="$smoke/empty-path" \
FILESAGE_ROOT="$smoke/root" \
FILESAGE_DATA_DIR="$smoke/data" \
FILESAGE_WEB_DIR="$package/web" \
FILESAGE_LISTEN_ADDR="127.0.0.1:$port" \
  "$package/filesage" > "$smoke/server.log" 2>&1 &
server_pid=$!

ready=0
for attempt in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/health" -o "$smoke/health.json" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
test "$ready" = 1
grep -q '"status":"ok"' "$smoke/health.json"
curl -fsS "http://127.0.0.1:$port/" -o "$smoke/index.html"

curl -fsS --request POST \
  --header 'Content-Type: application/json' \
  --data "{\"parent\":\"\",\"name\":\"$name\"}" \
  "http://127.0.0.1:$port/api/v1/files/directories" > "$smoke/create.json"

listed=0
for attempt in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/files?path=" -o "$smoke/list.json" \
    && grep -q "$name" "$smoke/list.json"; then
    listed=1
    break
  fi
  sleep 1
done
test "$listed" = 1
test -d "$smoke/root/$name"
echo "Release package smoke test passed."
