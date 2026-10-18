"""核验采集资料的来源、内容校验值和项目自写文档链接。"""

import hashlib
import json
import re
from collections import Counter
from datetime import datetime
from pathlib import Path
from urllib.parse import unquote
from zoneinfo import ZoneInfo


ROOT = Path(__file__).resolve().parents[1]


def verify():
    """逐项核对实际文件，并将结果保存为可复查报告。"""
    manifest = json.loads((ROOT / "manifest.json").read_text(encoding="utf-8"))
    errors = []
    projects = manifest["projects"]
    total_bytes = 0
    total_files = 0
    supplemental_files = 0
    linked = 0
    repositories = set()
    for project in projects:
        repo = project["repository"]
        if repo in repositories:
            errors.append(f"重复仓库：{repo}")
        repositories.add(repo)
        if not project.get("snapshot_commit"):
            errors.append(f"没有固定提交：{repo}")
        if not project["readme_available"]:
            errors.append(f"没有 README：{repo}")
        for item in project["files"]:
            file = (ROOT / item["local_path"]).resolve()
            file.relative_to(ROOT.resolve())
            if not file.is_file():
                errors.append(f"文件缺失：{item['local_path']}")
                continue
            content = file.read_bytes()
            total_bytes += len(content)
            total_files += 1
            if len(content) != item["bytes"] or hashlib.sha256(content).hexdigest() != item["sha256"]:
                errors.append(f"内容校验不一致：{item['local_path']}")
            if f"/{project['snapshot_commit']}/" not in item["source_url"]:
                errors.append(f"来源未固定提交：{item['local_path']}")
    official_manifest = ROOT / "official/manifest.json"
    official_pages = json.loads(official_manifest.read_text(encoding="utf-8")) if official_manifest.exists() else []
    for page in official_pages:
        if page.get("error"):
            errors.append(f"官方页面采集失败：{page['name']}")
        for item in page["files"]:
            file = (ROOT / item["local_path"]).resolve()
            file.relative_to(ROOT.resolve())
            if not file.exists():
                errors.append(f"官方文件缺失：{item['local_path']}")
                continue
            content = file.read_bytes()
            supplemental_files += 1
            if len(content) != item["bytes"] or hashlib.sha256(content).hexdigest() != item["sha256"]:
                errors.append(f"官方文件校验不一致：{item['local_path']}")
    context_files = sorted((ROOT / "context7").glob("*.json"))
    for file in context_files:
        snapshot = json.loads(file.read_text(encoding="utf-8"))
        if not snapshot.get("provider") or not snapshot.get("result", {}).get("content"):
            errors.append(f"文档查询快照不完整：{file.name}")
    authored = [ROOT / "README.md", ROOT / "catalog.md", ROOT / "research.md",
                ROOT / "technical-coverage.md", ROOT.parent / "docs/PRD.md",
                ROOT.parent / "docs/TECH.md", ROOT.parent / "docs/design/UIUX.md",
                ROOT.parent / "docs/design/prototype/README.md"]
    authored.extend(sorted((ROOT.parent / "docs/design/adr").glob("*.md")))
    for document in authored:
        content = document.read_text(encoding="utf-8")
        for target in re.findall(r"\]\(([^)]+)\)", content):
            if re.match(r"https?://|#", target):
                continue
            target = unquote(target.split("#")[0].strip("<>"))
            linked += 1
            if not (document.parent / target).resolve().exists():
                errors.append(f"链接失效：{document.relative_to(ROOT.parent)} -> {target}")
    trees = sum("tree_commit" in project for project in projects)
    categories = Counter(project["category"] for project in projects)
    missing_license = [project["repository"] for project in projects if not any(
        re.match(r"^(LICENSE|COPYING)", Path(item["path"]).name, re.I) for item in project["files"])]
    checked_on = datetime.now(ZoneInfo("Asia/Shanghai")).date().isoformat()
    report = ["# 调研资料核验", "", f"核验日期：{checked_on}（Asia/Shanghai）。资料采集日期见来源清单。", "",
              f"结果：{'通过' if not errors else '未通过'}。", "",
              f"- 仓库：{len(projects)}，README：{sum(project['readme_available'] for project in projects)}。",
              f"- 固定提交文档：{total_files}，原文字节：{total_bytes:,}。",
              f"- 重点仓库目录树：{trees}。",
              f"- 官方页面：{len(official_pages)}，HTML／正文文件：{supplemental_files}，均核对字节长度与 SHA-256。",
              f"- 文档查询快照：{len(context_files)}，JSON 结构与内容检查通过。",
              f"- 自写文档本地链接检查：{linked}。",
              "- 文件逐项核对存在性、字节长度、SHA-256 与固定提交来源。",
              "", "## 分类数量", "", "| 类别 | 仓库数 |", "| --- | --- |"]
    report.extend(f"| {category} | {count} |" for category, count in sorted(categories.items()))
    report += ["", "## 证据限制", "", "竞品未部署运行；文档能力声明不等于实测通过。上游文件原文、注释和版本描述按原样保留，自写代码注释采用中文。上游文档内未采集外链不参与完整性检查。",
               "", "未采到独立许可证文件的仓库：" + ("、".join(missing_license) or "无") + "。缺少独立文件不代表没有许可证，复用实现前需检查上游。",
               "", "候选文件不存在或读取失败的记录保留在 sources.json 的 unavailable 中；这些记录不等于核心采集失败。", "",
               "## 问题", ""]
    report += [f"- {error}" for error in errors] if errors else ["无文件完整性或自写文档链接问题。"]
    (ROOT / "verification.md").write_text("\n".join(report) + "\n", encoding="utf-8")
    print(json.dumps({"projects": len(projects), "files": total_files, "trees": trees,
                      "official_pages": len(official_pages), "official_files": supplemental_files,
                      "context_snapshots": len(context_files),
                      "local_links": linked, "bytes": total_bytes, "errors": errors}, ensure_ascii=False, indent=2))
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(verify())
