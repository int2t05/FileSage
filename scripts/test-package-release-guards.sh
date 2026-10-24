#!/usr/bin/env bash
set -euo pipefail

# 在隔离 Git 仓库中验证发布路径拒绝外部软链接。
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
package_script="$script_dir/package-release.sh"
tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/filesage-package-guard.XXXXXX")"
trap 'rm -rf -- "$tmp_root"' EXIT

new_repo() {
  local repo="$1"
  mkdir -p "$repo/scripts"
  cp "$package_script" "$repo/scripts/package-release.sh"
  git -C "$repo" init -q
  git -C "$repo" config user.name "FileSage CI"
  git -C "$repo" config user.email "filesage-ci@example.invalid"
  git -C "$repo" add scripts/package-release.sh
  git -C "$repo" commit -qm "guard fixture"
}

rejects() {
  local repo="$1"
  local expected="$2"
  local output
  local status

  set +e
  output="$(cd "$repo" && bash scripts/package-release.sh v1.2.3 HEAD 2>&1)"
  status=$?
  set -e
  if [ "$status" -eq 0 ] || [[ "$output" != *"$expected"* ]]; then
    printf 'expected packaging rejection containing %q; exit=%s output=%s\n' "$expected" "$status" "$output" >&2
    return 1
  fi
}

add_web_licenses() {
  local repo="$1"
  mkdir -p "$repo/web/dist/LICENSES/npm" "$repo/dist"
  printf 'notices\n' > "$repo/web/dist/THIRD_PARTY_NOTICES.md"
  printf 'license\n' > "$repo/web/dist/LICENSES/npm/example.txt"
}

repo="$tmp_root/web-parent"
external="$tmp_root/external-web"
new_repo "$repo"
mkdir -p "$external/dist"
ln -s "$external" "$repo/web"
rejects "$repo" "symlinked web directory"

repo="$tmp_root/web-dist-root"
external="$tmp_root/external-dist"
new_repo "$repo"
mkdir -p "$repo/web" "$external"
ln -s "$external" "$repo/web/dist"
rejects "$repo" "symlinked web directory"

repo="$tmp_root/web-content"
external="$tmp_root/external-content"
new_repo "$repo"
add_web_licenses "$repo"
mkdir -p "$external"
ln -s "$external" "$repo/web/dist/external"
rejects "$repo" "symlink or special file in web/dist"

repo="$tmp_root/web-special"
new_repo "$repo"
add_web_licenses "$repo"
mkfifo "$repo/web/dist/stream"
rejects "$repo" "symlink or special file in web/dist"

repo="$tmp_root/binary-input"
new_repo "$repo"
add_web_licenses "$repo"
printf 'external binary\n' > "$tmp_root/outside-binary"
ln -s "$tmp_root/outside-binary" "$repo/dist/filesage"
rejects "$repo" "symlinked or missing dist/filesage"
grep -qx 'external binary' "$tmp_root/outside-binary"

for output in filesage_v1.2.3_linux_amd64.tar.gz filesage_v1.2.3_linux_amd64.tar.gz.sha256; do
  repo="$tmp_root/output-${output##*.}"
  marker="$tmp_root/outside-${output##*.}"
  new_repo "$repo"
  add_web_licenses "$repo"
  printf 'binary fixture\n' > "$repo/dist/filesage"
  printf 'unchanged\n' > "$marker"
  ln -s "$marker" "$repo/dist/$output"
  rejects "$repo" "refusing to overwrite a symlink"
  grep -qx 'unchanged' "$marker"
done

echo "Release package symlink guards passed."
