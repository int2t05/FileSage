"""Fetch a small, attributed Lucide SVG sample for the FileSage icon study."""

import hashlib
import json
import urllib.parse

from collect import ROOT, fetch


REPOSITORY = "lucide-icons/lucide"
ICONS = [
    "folder", "folder-open", "file", "file-text", "file-image", "file-archive",
    "search", "upload", "download", "hard-drive", "clock-3", "settings-2",
    "check", "triangle-alert", "copy", "move", "pencil", "arrow-up-down",
    "list", "grid-2x2", "image", "play", "shield-check", "refresh-cw", "sun", "moon",
]


def main():
    source = ROOT / "projects/frontend-design/lucide-icons__lucide/sources.json"
    record = json.loads(source.read_text(encoding="utf-8"))
    commit = record["snapshot_commit"]
    known = {item["path"] for item in record["files"]}
    for name in ICONS:
        path = f"icons/{name}.svg"
        if path in known:
            continue
        url = f"https://raw.githubusercontent.com/{REPOSITORY}/{commit}/{urllib.parse.quote(path, safe='/')}"
        content, resolved = fetch(url)
        output = source.parent / "upstream" / path
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(content)
        record["files"].append({
            "path": path,
            "source_url": resolved,
            "local_path": output.relative_to(ROOT).as_posix(),
            "bytes": len(content),
            "sha256": hashlib.sha256(content).hexdigest(),
        })
    record["files"].sort(key=lambda item: item["path"])
    source.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"Saved {len(ICONS)} Lucide SVG samples from {commit}")


if __name__ == "__main__":
    main()
