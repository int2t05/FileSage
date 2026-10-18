"""读取重点仓库的固定提交目录树，补充与需求有关的文档。"""

import concurrent.futures
import hashlib
import json
import re
import subprocess
import tempfile
import urllib.parse

from collect import ROOT, fetch, main as rebuild_catalog


FOCUS = {
    "gtsteffaniak/filebrowser", "quantumx-apps/filebrowserDocs", "9001/copyparty",
    "nxzai/NextExplorer", "MrCodeEU/goDrive", "sigoden/dufs", "mickael-kerjean/filestash",
    "immich-app/immich", "bpatrik/pigallery2", "photoview/photoview", "photoprism/photoprism",
    "paperless-ngx/paperless-ngx", "eikek/docspell", "papermerge/papermerge-core",
    "clusterzx/paperless-ai", "icereed/paperless-gpt", "jazmy/genai-file-organizer",
    "jjuliano/aifiles", "CrispStrobe/CrispSorter", "jagas32/smart-file-organizer",
    "SysAdminDoc/FileOrganizer", "caiowilson/ClutterCut", "tfeldmann/organize",
    "qarmin/czkawka", "sahib/rmlint", "transloadit/uppy", "tus/tusd",
    "haiwen/seafile", "pulsejet/memories",
    "futurepress/epub.js", "SheetJS/sheetjs", "VolodymyrBaydalka/docxjs",
    "meshesha/PPTXjs", "gildas-lormeau/zip.js", "libarchive/libarchive",
    "nika-begiashvili/libarchivejs", "codemirror/website", "cure53/DOMPurify",
    "python-openxml/python-docx", "scanny/python-pptx", "exceljs/exceljs",
    "py-pdf/pypdf", "docling-project/docling", "tesseract-ocr/tesseract",
    "containers/bubblewrap", "google/nsjail", "anthropics/sandbox-runtime",
    "google/safeopen", "fsnotify/fsnotify", "mattn/go-sqlite3", "modernc-org/sqlite",
    "hibiken/asynq", "fastapi/fastapi", "tokio-rs/axum",
    "vuejs/core", "facebook/react", "vitejs/vite", "ollama/ollama",
    "ggml-org/llama.cpp", "libvips/libvips", "mholt/archives", "tus/tus-js-client",
    "lucide-icons/lucide", "tabler/tabler-icons", "phosphor-icons/core",
    "radix-ui/primitives", "shadcn-ui/ui", "primefaces/primevue",
    "element-plus/element-plus", "ant-design/ant-design",
}


def deepen(source):
    """仅下载文档树中的相关文本，临时 Git 索引随上下文清理。"""
    record = json.loads(source.read_text(encoding="utf-8"))
    repo = record["repository"]
    commit = record.get("snapshot_commit")
    if repo not in FOCUS or not commit:
        return
    try:
        tree_path = source.parent / "repository-tree.txt"
        if tree_path.exists() and record.get("tree_commit") == commit:
            listing = tree_path.read_text(encoding="utf-8")
        else:
            with tempfile.TemporaryDirectory(prefix="filesage-research-") as temporary:
                subprocess.run(["git", "init", "--bare", temporary], capture_output=True, check=True, timeout=15)
                subprocess.run(["git", "-C", temporary, "fetch", "--depth=1", "--filter=blob:none",
                                record["url"] + ".git", commit], capture_output=True, check=True, timeout=90)
                listing = subprocess.run(["git", "-C", temporary, "ls-tree", "-r", "--name-only", commit],
                                         capture_output=True, text=True, check=True, timeout=30).stdout
            tree_path.write_text(listing, encoding="utf-8")
        record["tree_commit"] = commit
        paths = listing.splitlines()
        available = {item["path"] for item in record["files"]}
        candidates = [path for path in paths if re.search(r"\.(md|mdx|rst)$", path, re.I)
                      and not re.search(r"changelog|release.notes|contribut|code.of.conduct|locale|i18n|/zh/|/es/|/fr/|/ja/|/de/|/ru/|/test", path, re.I)
                      and (re.search(r"(^|/)(docs?|documentation|guides?|content|website)(/|$)", path, re.I)
                           or re.search(r"(^|/)(README|LICENSE)[^/]*$", path, re.I))]
        keywords = r"upload|resum|duplicate|external.libr|storage|filesystem|preview|file.manage|source|trash|undo|approv|review|safe|plan|ocr|usage|search|performance|backup|metadata|config|feature|render|navigation|worksheet|slide|extract|edit|sandbox"
        candidates.sort(key=lambda path: (-len(re.findall(keywords, path, re.I)), len(path), path))
        for path in [path for path in candidates if path not in available][:30]:
            url = f"https://raw.githubusercontent.com/{repo}/{commit}/{urllib.parse.quote(path, safe='/')}"
            try:
                content, resolved = fetch(url)
                output = (source.parent / "upstream" / path).resolve()
                output.relative_to(source.parent.resolve())
                output.parent.mkdir(parents=True, exist_ok=True)
                output.write_bytes(content)
                record["files"].append({"path": path, "source_url": resolved,
                    "local_path": output.relative_to(ROOT).as_posix(), "bytes": len(content),
                    "sha256": hashlib.sha256(content).hexdigest()})
                if re.match(r"^readme", path, re.I):
                    record["readme_available"] = True
            except Exception as error:
                record["unavailable"].append({"resource": path, "reason": str(error)[:240]})
        print(f"deep {repo}: {len(record['files'])} files", flush=True)
    except (subprocess.SubprocessError, OSError) as error:
        record["unavailable"].append({"resource": "repository-tree", "reason": str(error)[:240]})
        print(f"deep {repo}: tree unavailable", flush=True)
    source.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")


def main():
    """并发补充重点项目资料并刷新总清单。"""
    manifest = json.loads((ROOT / "manifest.json").read_text(encoding="utf-8"))
    sources = [ROOT / "projects" / item["category"] / item["repository"].replace("/", "__") / "sources.json"
               for item in manifest["projects"] if "tree_commit" not in item]
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        list(pool.map(deepen, sources))
    rebuild_catalog()


if __name__ == "__main__":
    main()
