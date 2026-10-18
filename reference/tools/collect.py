"""采集公开 GitHub 项目的文档快照，保留提交、来源及完整性记录。"""

import concurrent.futures
import hashlib
import json
import posixpath
import re
import subprocess
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from zoneinfo import ZoneInfo


ROOT = Path(__file__).resolve().parents[1]
PROJECTS = {
    "file-management": [
        "filebrowser/filebrowser", "gtsteffaniak/filebrowser", "9001/copyparty",
        "mickael-kerjean/filestash", "nxzai/NextExplorer", "sigoden/dufs",
        "lrsjng/h5ai", "pldubouilh/gossa", "codeskyblue/gohttpserver",
        "Cloudreve/Cloudreve", "AlistGo/alist", "OpenListTeam/OpenList",
        "kalcaddle/KodExplorer", "kalcaddle/kodbox", "drakkan/sftpgo",
        "hacdias/webdav", "B3llo/the-filebrowser", "MrCodeEU/goDrive",
    ],
    "personal-cloud": [
        "nextcloud/server", "owncloud/core", "pydio/cells", "haiwen/seafile",
        "syncthing/syncthing", "rclone/rclone",
    ],
    "photo-management": [
        "immich-app/immich", "photoprism/photoprism", "bpatrik/pigallery2",
        "photoview/photoview", "LibrePhotos/librephotos", "LycheeOrg/Lychee",
        "Piwigo/Piwigo", "ente-io/ente", "xemle/home-gallery", "saimn/sigal",
        "pulsejet/memories",
    ],
    "document-management": [
        "paperless-ngx/paperless-ngx", "papermerge/papermerge-core",
        "mayan-edms/mayan-edms", "sismics/docs", "eikek/docspell",
        "Stirling-Tools/Stirling-PDF",
    ],
    "ai-organization": [
        "clusterzx/paperless-ai", "icereed/paperless-gpt",
        "jazmy/genai-file-organizer", "jjuliano/aifiles",
        "CrispStrobe/CrispSorter", "jagas32/smart-file-organizer",
        "youi2000jp4/ai-file-organizer", "SysAdminDoc/FileOrganizer",
        "ShichengRao/smart-file-organizer", "caiowilson/ClutterCut",
        "SeyiDan/smart-file-organizer", "tfeldmann/organize",
    ],
    "implementation-reference": [
        "qarmin/czkawka", "sahib/rmlint", "adrianlopezroche/fdupes",
        "exiftool/exiftool", "transloadit/uppy", "tus/tusd",
        "mozilla/pdf.js", "markedjs/marked", "quantumx-apps/filebrowserDocs", "tus/tus-js-client",
    ],
    "technical-reference": [
        "futurepress/epub.js", "SheetJS/sheetjs", "VolodymyrBaydalka/docxjs",
        "meshesha/PPTXjs", "ONLYOFFICE/DocumentServer", "ONLYOFFICE/sdkjs",
        "CollaboraOnline/online", "LibreOffice/core", "gildas-lormeau/zip.js",
        "libarchive/libarchive", "nika-begiashvili/libarchivejs",
        "codemirror/website", "microsoft/monaco-editor", "cure53/DOMPurify",
        "python-openxml/python-docx", "scanny/python-pptx", "exceljs/exceljs",
        "gitbrent/PptxGenJS", "py-pdf/pypdf", "docling-project/docling",
        "Unstructured-IO/unstructured", "tesseract-ocr/tesseract",
        "containers/bubblewrap", "google/nsjail", "anthropics/sandbox-runtime",
        "google/safeopen", "fsnotify/fsnotify", "mattn/go-sqlite3",
        "modernc-org/sqlite", "hibiken/asynq",
        "fastapi/fastapi", "tokio-rs/axum", "vuejs/core", "facebook/react",
        "vitejs/vite", "ollama/ollama", "ggml-org/llama.cpp",
        "libvips/libvips", "mholt/archives",
    ],
    "frontend-design": [
        "lucide-icons/lucide", "tabler/tabler-icons", "phosphor-icons/core",
        "radix-ui/primitives", "shadcn-ui/ui", "primefaces/primevue",
        "element-plus/element-plus", "ant-design/ant-design",
    ],
}


def fetch(url):
    """读取公开文本资源，限制大小，拒绝错误页及二进制内容。"""
    request = urllib.request.Request(url, headers={"User-Agent": "FileSage-public-research"})
    with urllib.request.urlopen(request, timeout=25) as response:
        content = response.read(2_000_001)
        if len(content) > 2_000_000:
            raise ValueError("资源超过文档大小限制")
        if b"\x00" in content:
            raise ValueError("资源为二进制")
        return content, response.geturl()


def collect_project(category, repo):
    """定位默认分支并采集入口文档及其关联说明，逐项记录失败。"""
    destination = ROOT / "projects" / category / repo.replace("/", "__")
    destination.mkdir(parents=True, exist_ok=True)
    existing = destination / "sources.json"
    if existing.exists():
        cached = json.loads(existing.read_text(encoding="utf-8"))
        if cached.get("readme_available") and cached.get("snapshot_commit"):
            return cached
    record = {"repository": repo, "category": category,
              "url": f"https://github.com/{repo}", "retrieved_at": datetime.now(timezone.utc).isoformat(),
              "files": [], "unavailable": [], "snapshot_commit": None, "default_branch": None}
    try:
        result = subprocess.run(["git", "ls-remote", "--symref", record["url"] + ".git", "HEAD"],
                                capture_output=True, text=True, timeout=40, check=True)
        for line in result.stdout.splitlines():
            if line.startswith("ref: refs/heads/"):
                record["default_branch"] = line.split("\t")[0].removeprefix("ref: refs/heads/")
            elif re.match(r"^[0-9a-f]{40}\s+HEAD$", line):
                record["snapshot_commit"] = line.split()[0]
    except (subprocess.SubprocessError, OSError) as error:
        record["unavailable"].append({"resource": "HEAD", "reason": str(error)[:240]})
    if not record["snapshot_commit"]:
        try:
            metadata, _ = fetch(f"https://api.github.com/repos/{repo}")
            record["default_branch"] = json.loads(metadata)["default_branch"]
            branch = urllib.parse.quote(record["default_branch"], safe="/")
            reference, _ = fetch(f"https://api.github.com/repos/{repo}/git/ref/heads/{branch}")
            commit = json.loads(reference)["object"]
            if commit["type"] != "commit" or not re.fullmatch(r"[0-9a-f]{40}", commit["sha"]):
                raise ValueError("默认分支未返回有效提交")
            record["snapshot_commit"] = commit["sha"]
            record["commit_resolution"] = "github-rest"
        except (urllib.error.URLError, TimeoutError, ValueError, KeyError, OSError) as error:
            record["unavailable"].append({"resource": "default-branch-commit", "reason": str(error)[:240]})
    if not record["snapshot_commit"]:
        record["readme_available"] = False
        existing.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"{repo}: commit unavailable", flush=True)
        return record
    ref = record["snapshot_commit"]
    saved = set()

    def save(path):
        """保存上游原文及其来源，确保本地目标始终位于项目目录。"""
        path = posixpath.normpath(urllib.parse.unquote(path)).lstrip("/")
        if path in saved or path.startswith("../") or ":" in path or "\\" in path:
            return None
        url = f"https://raw.githubusercontent.com/{repo}/{ref}/{urllib.parse.quote(path, safe='/')}"
        try:
            content, resolved = fetch(url)
            output = (destination / "upstream" / path).resolve()
            output.relative_to(destination.resolve())
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(content)
            saved.add(path)
            record["files"].append({"path": path, "source_url": resolved,
                                    "local_path": output.relative_to(ROOT).as_posix(),
                                    "bytes": len(content), "sha256": hashlib.sha256(content).hexdigest()})
            return content.decode("utf-8", errors="replace")
        except (urllib.error.URLError, TimeoutError, ValueError, OSError) as error:
            record["unavailable"].append({"resource": path, "reason": str(error)[:240]})
            return None

    readme = None
    readme_path = None
    for candidate in ["README.md", "readme.md", "README.rst", "README", "Readme.md", "README.markdown"]:
        readme = save(candidate)
        if readme is not None:
            readme_path = candidate
            break
    for candidate in ["LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING", "COPYING.md"]:
        if save(candidate) is not None:
            break
    if readme:
        candidates = set()
        for link in re.findall(r"\]\(([^\s)]+)", readme):
            link = link.split("#")[0].split("?")[0]
            if not re.search(r"\.(md|rst|txt)$", link, re.I):
                continue
            if link.startswith("http"):
                match = re.match(rf"https://github.com/{re.escape(repo)}/blob/[^/]+/(.+)", link)
                if match:
                    candidates.add(match.group(1))
            else:
                candidates.add(posixpath.normpath(posixpath.join(posixpath.dirname(readme_path), link)))
        ranked = sorted(candidates, key=lambda path: (
            not bool(re.search(r"feature|usage|config|upload|safe|undo|guide|install|deploy|doc|readme", path, re.I)), path))
        for path in ranked[:10]:
            if not re.search(r"contribut|code.of.conduct|changelog|translation", path, re.I):
                save(path)
    record["readme_available"] = readme is not None
    (destination / "sources.json").write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"{repo}: {len(record['files'])} files, readme={record['readme_available']}", flush=True)
    return record


def main():
    """并发采集独立项目，生成可核验总清单和分类索引。"""
    candidates = [(category, repo) for category, repos in PROJECTS.items() for repo in repos]
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        jobs = [pool.submit(collect_project, category, repo) for category, repo in candidates]
        for job in concurrent.futures.as_completed(jobs):
            results.append(job.result())
    results.sort(key=lambda item: (item["category"], item["repository"].lower()))
    research_date = datetime.now(ZoneInfo("Asia/Shanghai")).date().isoformat()
    (ROOT / "manifest.json").write_text(json.dumps({"research_date": research_date,
        "timezone": "Asia/Shanghai", "projects": results}, ensure_ascii=False, indent=2), encoding="utf-8")
    lines = ["# GitHub 项目索引", "", f"采集日期：{research_date}（Asia/Shanghai）。上游能力声明未经运行验证；文件来源及校验值见各项目 sources.json。", "",
             "| 类别 | 项目 | 提交定位 | README | 文件数 | 本地资料 |", "| --- | --- | --- | --- | --- | --- |"]
    for item in results:
        folder = f"projects/{item['category']}/{item['repository'].replace('/', '__')}"
        commit = item["snapshot_commit"]
        pointer = f"[{commit[:8]}]({item['url']}/tree/{commit})" if commit else "定位失败"
        lines.append(f"| {item['category']} | [{item['repository']}]({item['url']}) | {pointer} | {'成功' if item['readme_available'] else '未取得'} | {len(item['files'])} | [来源]({folder}/sources.json) |")
    (ROOT / "catalog.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(json.dumps({"projects": len(results), "readmes": sum(item["readme_available"] for item in results),
                      "files": sum(len(item["files"]) for item in results)}, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
