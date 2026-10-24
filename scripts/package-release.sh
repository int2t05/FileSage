#!/usr/bin/env bash
set -euo pipefail

# 生成可复现的 Linux amd64 发布包。
if [ "$#" -ne 2 ]; then
  echo "usage: package-release.sh vMAJOR.MINOR.PATCH COMMIT" >&2
  exit 2
fi

version="$1"
commit="$2"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "release version must use vMAJOR.MINOR.PATCH" >&2
  exit 2
fi
git cat-file -e "$commit^{commit}"

# 发布包只接收真实构建目录及其普通文件。
if [ -L web ] || [ -L web/dist ] || [ ! -d web/dist ]; then
  echo "refusing to package a symlinked web directory or missing web/dist" >&2
  exit 1
fi
web_unsupported="$(find web/dist -mindepth 1 ! -type d ! -type f -print -quit)"
if [ -n "$web_unsupported" ]; then
  echo "refusing to package a symlink or special file in web/dist" >&2
  exit 1
fi

# 先校验工具链，避免生成缺少构建信息的发布包。
go_version="$(go version)"
node_version="$(node --version)"
npm_version="$(npm --version)"
if [ ! -s web/dist/THIRD_PARTY_NOTICES.md ] \
  || [ ! -d web/dist/LICENSES/npm ] \
  || [ -z "$(find web/dist/LICENSES/npm -type f -print -quit 2>/dev/null)" ]; then
  echo "frontend dependency licenses are missing; build the web app first" >&2
  exit 1
fi

bundle="filesage_"$version"_linux_amd64"
stage="dist/$bundle"
archive="$bundle.tar.gz"
checksum="$archive.sha256"
case "$stage" in
  dist/filesage_v*_linux_amd64) ;;
  *) echo "invalid package path" >&2; exit 2 ;;
esac

# 发布包只写入仓库内的真实输出目录，避免沿符号链接清理外部文件。
if [ -L dist ]; then
  echo "refusing to package through a dist symlink" >&2
  exit 1
fi

mkdir -p dist
if [ -L dist/filesage ] || [ ! -f dist/filesage ]; then
  echo "refusing to package a symlinked or missing dist/filesage" >&2
  exit 1
fi
for output in "$archive" "$checksum"; do
  output_path="dist/$output"
  if [ -L "$output_path" ] || { [ -e "$output_path" ] && [ ! -f "$output_path" ]; }; then
    printf 'refusing to overwrite a symlink or non-file: %s\n' "$output_path" >&2
    exit 1
  fi
done
rm -rf -- "$stage"
mkdir -p "$stage/web" "$stage/deploy" "$stage/docs" "$stage/LICENSES"
install -m 0755 dist/filesage "$stage/filesage"
cp -R web/dist/. "$stage/web/"
cp deploy/filesage.service deploy/filesage.env.example "$stage/deploy/"
cp docs/DEPLOYMENT.md docs/agent-conversation-api.md "$stage/docs/"
cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"
cp -R LICENSES/. "$stage/LICENSES/"
bash scripts/collect-go-licenses.sh "$stage/LICENSES"

{
  printf 'Version: %s\n' "$version"
  printf 'Commit: %s\n' "$commit"
  printf 'Go: %s\n' "$go_version"
  printf 'Node: %s\n' "$node_version"
  printf 'npm: %s\n' "$npm_version"
} > "$stage/BUILDINFO.txt"

source_date_epoch="$(git show -s --format=%ct "$commit")"
tar --sort=name --mtime="@$source_date_epoch" --owner=0 --group=0 --numeric-owner -cf - -C dist "$bundle" | gzip -n > "dist/$archive"
(cd dist && sha256sum "$archive" > "$checksum")
tar -tzf "dist/$archive" >/dev/null
(cd dist && sha256sum -c "$checksum" >&2)

printf 'archive=dist/%s\n' "$archive"
printf 'checksum=dist/%s\n' "$checksum"
