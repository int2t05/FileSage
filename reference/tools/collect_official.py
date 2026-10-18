"""补充官方格式文档和 Linux 文件操作手册，保留原始页面及校验记录。"""

import concurrent.futures
import hashlib
import json
from datetime import datetime, timezone
from html.parser import HTMLParser

from collect import ROOT, fetch


PAGES = {
    "libreoffice-conversion": "https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html",
    "sheetjs-html": "https://docs.sheetjs.com/docs/api/utilities/html/",
    "linux-openat2": "https://man7.org/linux/man-pages/man2/openat2.2.html",
    "linux-rename": "https://man7.org/linux/man-pages/man2/rename.2.html",
    "linux-chmod": "https://man7.org/linux/man-pages/man2/chmod.2.html",
    "linux-chown": "https://man7.org/linux/man-pages/man2/chown.2.html",
    "linux-symlink": "https://man7.org/linux/man-pages/man7/symlink.7.html",
    "linux-landlock": "https://man7.org/linux/man-pages/man7/landlock.7.html",
    "http-conditional-requests": "https://www.rfc-editor.org/rfc/rfc9110.html",
    "sqlite-wal": "https://www.sqlite.org/wal.html",
    "sqlite-fts5": "https://www.sqlite.org/fts5.html",
    "go-osroot": "https://go.dev/blog/osroot",
    "go-release": "https://go.dev/doc/devel/release",
    "go-root-vulnerability": "https://pkg.go.dev/vuln/GO-2026-4970",
    "linux-inotify": "https://man7.org/linux/man-pages/man7/inotify.7.html",
    "owasp-csrf": "https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html",
    "tus-protocol": "https://tus.io/protocols/resumable-upload.html",
    "pdfjs-getting-started": "https://mozilla.github.io/pdf.js/getting_started/?lang=en",
    "onlyoffice-license": "https://github.com/ONLYOFFICE/DocumentServer/blob/master/LICENSE",
    "radix-accessibility": "https://www.radix-ui.com/primitives/docs/overview/accessibility",
    "radix-introduction": "https://www.radix-ui.com/primitives/docs/overview/introduction",
    "tabler-license": "https://tabler.io/license",
    "lucide-icons": "https://lucide.dev/guide/",
    "phosphor-icons": "https://phosphoricons.com/",
    "shadcn-ui": "https://ui.shadcn.com/docs",
    "primevue": "https://primevue.org/",
}


class PageText(HTMLParser):
    """提取可读文本供查阅，保留 HTML 原件作为出处。"""

    def __init__(self):
        super().__init__()
        self.hidden = 0
        self.parts = []

    def handle_starttag(self, tag, attrs):
        """排除脚本样式，在块边界保留换行。"""
        if tag in {"script", "style"}:
            self.hidden += 1
        elif tag in {"p", "div", "tr", "br", "li", "h1", "h2", "h3", "pre"}:
            self.parts.append("\n")

    def handle_endtag(self, tag):
        """结束不可读区域并分隔表格单元格。"""
        if tag in {"script", "style"}:
            self.hidden = max(0, self.hidden - 1)
        elif tag in {"td", "th"}:
            self.parts.append(" | ")

    def handle_data(self, data):
        """收集普通正文，不执行页面内容。"""
        if not self.hidden:
            self.parts.append(data)


def collect_page(item):
    """保存单页原文和便于阅读的正文，失败结果明确登记。"""
    name, url = item
    destination = ROOT / "official"
    destination.mkdir(parents=True, exist_ok=True)
    record = {"name": name, "url": url, "retrieved_at": datetime.now(timezone.utc).isoformat(), "files": []}
    try:
        content, resolved = fetch(url)
        record["source_url"] = resolved
        parser = PageText()
        parser.feed(content.decode("utf-8", errors="replace"))
        text = "\n".join(line.strip() for line in "".join(parser.parts).splitlines() if line.strip()) + "\n"
        for suffix, data in [("html", content), ("txt", text.encode("utf-8"))]:
            path = destination / f"{name}.{suffix}"
            path.write_bytes(data)
            record["files"].append({"local_path": path.relative_to(ROOT).as_posix(), "bytes": len(data),
                                    "sha256": hashlib.sha256(data).hexdigest()})
        print(f"official {name}: saved", flush=True)
    except Exception as error:
        record["error"] = str(error)
        print(f"official {name}: unavailable", flush=True)
    return record


def main():
    """采集独立官方页面并生成补充来源清单。"""
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        records = list(pool.map(collect_page, PAGES.items()))
    (ROOT / "official/manifest.json").write_text(json.dumps(records, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    main()
