#!/usr/bin/env bash
set -euo pipefail

# 收集服务端二进制实际依赖的 Go 模块许可文本。
if [ "$#" -ne 1 ]; then
  echo "usage: collect-go-licenses.sh RELEASE_LICENSES_DIR" >&2
  exit 2
fi

output="$1/go"
mkdir -p "$output"
index="$output/README.md"
printf '# Go 模块许可清单\n\n服务端二进制直接或间接包含以下 Go 模块。\n\n' > "$index"

module_list="$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps -f '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}{{end}}' ./cmd/filesage | LC_ALL=C sort -u)"
module_count=0
while IFS='|' read -r module version module_dir; do
  [ -n "$module" ] || continue
  [ -n "$version" ] && [ -d "$module_dir" ] || {
    echo "invalid Go module entry: $module $version $module_dir" >&2
    exit 1
  }

  license_files=()
  while IFS= read -r -d '' file; do
    name="${file##*/}"
    lower_name="${name,,}"
    if [[ "$lower_name" =~ ^(license|licence|copying|notice|patents)([._-].*)?$ ]]; then
      license_files+=("$file")
    fi
  done < <(find "$module_dir" -maxdepth 1 -type f -print0 | LC_ALL=C sort -z)

  if [ "${#license_files[@]}" -eq 0 ]; then
    echo "Go module has no top-level license text: $module@$version" >&2
    exit 1
  fi

  directory_name="$(printf '%s@%s' "$module" "$version" | sed -E 's@/@+@g; s/[^[:alnum:]@._+-]/_/g')"
  destination="$output/$directory_name"
  mkdir -p "$destination"
  for file in "${license_files[@]}"; do
    install -m 0644 "$file" "$destination/${file##*/}"
  done

  printf '%s@%s: `%s/`\n\n' "$module" "$version" "$directory_name" >> "$index"
  module_count=$((module_count + 1))
done <<< "$module_list"

if [ "$module_count" -eq 0 ]; then
  echo "no third-party Go modules found" >&2
  exit 1
fi

printf 'Collected licenses for %s Go modules.\n' "$module_count"
