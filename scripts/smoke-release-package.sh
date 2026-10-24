#!/usr/bin/env bash
set -euo pipefail

# 解包并以空 PATH 验证 Linux 发布包的写入与重启恢复。
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

# 独占测试端口，避免把写入请求误发给其他本机服务。
port="${FILESAGE_SMOKE_PORT:-18089}"
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]]; then
  echo "FILESAGE_SMOKE_PORT must be an integer from 1024 to 65535" >&2
  exit 2
fi
port=$((10#$port))
if (( port < 1024 || port > 65535 )); then
  echo "FILESAGE_SMOKE_PORT must be an integer from 1024 to 65535" >&2
  exit 2
fi
port_in_use() {
  local listeners
  if command -v ss >/dev/null 2>&1; then
    if listeners="$(ss -H -ltn "sport = :$1" 2>/dev/null)"; then
      [ -n "$listeners" ]
      return
    fi
  fi
  (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
}
if port_in_use "$port"; then
  printf 'TCP port %s is already in use; set FILESAGE_SMOKE_PORT to a free port\n' "$port" >&2
  exit 1
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
root="$smoke/root"
data="$smoke/data"
tar -xzf "$archive" -C "$smoke/extract"
package="$smoke/extract/$bundle"
test -x "$package/filesage"
test -s "$package/web/index.html"
test -s "$package/deploy/filesage.service"
test -s "$package/deploy/filesage.env.example"
grep -Fxq 'UMask=0022' "$package/deploy/filesage.service"
test -s "$package/THIRD_PARTY_NOTICES.md"
test -s "$package/LICENSES/go/README.md"
test -s "$package/web/THIRD_PARTY_NOTICES.md"
test -d "$package/web/LICENSES/npm"
test -n "$(find "$package/LICENSES/go" -mindepth 2 -type f -print -quit)"
test -n "$(find "$package/web/LICENSES/npm" -type f -print -quit)"
grep -q 'golang.org/x/sys@' "$package/LICENSES/go/README.md"
grep -q 'lucide-react@' "$package/web/THIRD_PARTY_NOTICES.md"

name="release-smoke-$$"
start_server() {
  PATH="$smoke/empty-path" \
  FILESAGE_ROOT="$root" \
  FILESAGE_DATA_DIR="$data" \
  FILESAGE_WEB_DIR="$package/web" \
  FILESAGE_LISTEN_ADDR="127.0.0.1:$port" \
    "$package/filesage" >> "$smoke/server.log" 2>&1 &
  server_pid=$!
}

wait_for_server() {
  local ready=0
  for attempt in $(seq 1 30); do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      wait "$server_pid" 2>/dev/null || true
      server_pid=
      echo "FileSage test process exited before health check" >&2
      return 1
    fi
    if curl -fsS "http://127.0.0.1:$port/api/v1/health" -o "$smoke/health.json" 2>/dev/null; then
      ready=1
      break
    fi
    sleep 1
  done
  test "$ready" = 1
  grep -q '"status":"ok"' "$smoke/health.json"
}

start_server
wait_for_server
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

# 完成一个 TUS 上传，重启后验证已发布状态仍可续传查询。
upload_name="$name-upload.txt"
upload_data="FileSage package upload recovery"
upload_size="${#upload_data}"
filename_metadata="$(printf '%s' "$upload_name" | base64 | tr -d '\n')"
target_metadata="$(printf '%s' '.' | base64 | tr -d '\n')"
upload_create_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --dump-header "$smoke/upload-create.headers" \
  --request POST \
  --header 'Tus-Resumable: 1.0.0' \
  --header "Upload-Length: $upload_size" \
  --header "Upload-Metadata: filename $filename_metadata,target $target_metadata,relativepath $filename_metadata" \
  "http://127.0.0.1:$port/api/v1/uploads/")"
test "$upload_create_status" = 201
upload_url="$(awk 'tolower($1) == "location:" { sub("\r$", "", $2); print $2 }' "$smoke/upload-create.headers")"
test -n "$upload_url"
case "$upload_url" in
  http://*|https://*) ;;
  /*) upload_url="http://127.0.0.1:$port$upload_url" ;;
  *) upload_url="http://127.0.0.1:$port/$upload_url" ;;
esac
upload_id="${upload_url##*/}"
[[ "$upload_id" =~ ^[0-9a-f]{32}$ ]]
upload_patch_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --request PATCH \
  --header 'Tus-Resumable: 1.0.0' \
  --header 'Upload-Offset: 0' \
  --header 'Content-Type: application/offset+octet-stream' \
  --data-binary "$upload_data" \
  "$upload_url")"
test "$upload_patch_status" = 204
published=0
for attempt in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/uploads/status/$upload_id" -o "$smoke/upload-status-before.json" \
    && grep -q '"status":"published"' "$smoke/upload-status-before.json"; then
    published=1
    break
  fi
  sleep 1
done
test "$published" = 1
test ! -e "$data/upload-staging/$upload_id"
printf '%s' "$upload_data" | cmp - "$smoke/root/$upload_name"

kill -TERM "$server_pid"
wait "$server_pid"
server_pid=
start_server
wait_for_server

listed=0
for attempt in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:$port/api/v1/files?path=" -o "$smoke/list-after-restart.json" \
    && grep -q "$name" "$smoke/list-after-restart.json"; then
    listed=1
    break
  fi
  sleep 1
done
test "$listed" = 1
test -d "$smoke/root/$name"

upload_head_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --dump-header "$smoke/upload-head.headers" \
  --head \
  --header 'Tus-Resumable: 1.0.0' \
  "$upload_url")"
test "$upload_head_status" = 200
tr -d '\r' < "$smoke/upload-head.headers" > "$smoke/upload-head-normalized.headers"
grep -Eiq "^Upload-Offset:[[:space:]]*$upload_size$" "$smoke/upload-head-normalized.headers"
grep -Eiq "^Upload-Length:[[:space:]]*$upload_size$" "$smoke/upload-head-normalized.headers"
grep -Eiq '^Tus-Resumable:[[:space:]]*1\.0\.0$' "$smoke/upload-head-normalized.headers"
grep -Eiq '^Cache-Control:[[:space:]]*no-store$' "$smoke/upload-head-normalized.headers"
curl -fsS "http://127.0.0.1:$port/api/v1/uploads/status/$upload_id" -o "$smoke/upload-status-after.json"
grep -q '"status":"published"' "$smoke/upload-status-after.json"
grep -q "$upload_name" "$smoke/upload-status-after.json"
test "$(find "$smoke/root" -maxdepth 1 -type f -name "$upload_name" | wc -l)" -eq 1
printf '%s' "$upload_data" | cmp - "$smoke/root/$upload_name"

kill -TERM "$server_pid"
wait "$server_pid"
server_pid=

printf 'FileSage synthetic restore sample\n' > "$root/$name/restore.txt"
backup="$smoke/backup"
restore="$smoke/restore"
mkdir -p "$backup" "$restore/root" "$restore/data"
tar --acls --xattrs --numeric-owner -cpf "$backup/managed-root.tar" -C "$root" .
tar --acls --xattrs --numeric-owner -cpf "$backup/service-data.tar" -C "$data" .
(cd "$backup" && sha256sum managed-root.tar service-data.tar > SHA256SUMS)
(cd "$backup" && sha256sum -c SHA256SUMS)
tar --acls --xattrs --numeric-owner -xpf "$backup/managed-root.tar" -C "$restore/root"
tar --acls --xattrs --numeric-owner -xpf "$backup/service-data.tar" -C "$restore/data"
cmp "$root/$name/restore.txt" "$restore/root/$name/restore.txt"

root="$restore/root"
data="$restore/data"
start_server
wait_for_server

listed=0
for attempt in $(seq 1 20); do
  if curl -fsS --get --data-urlencode "path=$name" "http://127.0.0.1:$port/api/v1/files" -o "$smoke/list-after-restore.json" \
    && grep -q 'restore.txt' "$smoke/list-after-restore.json"; then
    listed=1
    break
  fi
  sleep 1
done
test "$listed" = 1
curl -fsS "http://127.0.0.1:$port/api/v1/uploads/status/$upload_id" -o "$smoke/upload-status-after-restore.json"
grep -q '"status":"published"' "$smoke/upload-status-after-restore.json"
grep -q "$upload_name" "$smoke/upload-status-after-restore.json"
printf '%s' "$upload_data" | cmp - "$root/$upload_name"
echo "Release package smoke test passed (start, write, upload, restart, backup, restore, read)."
