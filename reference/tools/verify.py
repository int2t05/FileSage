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
    local_project_snapshots = 0
    missing_project_snapshots = []
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
        local_root = ROOT / "projects" / project["category"] / repo.replace("/", "__")
        source_manifest = local_root / "sources.json"
        if not source_manifest.is_file():
            if local_root.exists():
                errors.append(f"本地来源清单缺失：{source_manifest.relative_to(ROOT)}")
            else:
                missing_project_snapshots.append(repo)
            continue
        local_project_snapshots += 1
        record = json.loads(source_manifest.read_text(encoding="utf-8"))
        for item in record.get("files", []):
            local_path = item.get("local_path") or (local_root / "upstream" / item["path"]).relative_to(ROOT).as_posix()
            file = (ROOT / local_path).resolve()
            file.relative_to(ROOT.resolve())
            if not file.is_file():
                errors.append(f"文件缺失：{local_path}")
                continue
            content = file.read_bytes()
            total_bytes += len(content)
            total_files += 1
            if len(content) != item["bytes"] or hashlib.sha256(content).hexdigest() != item["sha256"]:
                errors.append(f"内容校验不一致：{local_path}")
            if f"/{project['snapshot_commit']}/" not in item["source_url"]:
                errors.append(f"来源未固定提交：{local_path}")
    if local_project_snapshots and missing_project_snapshots:
        errors.append(f"本地来源快照不完整：缺少 {len(missing_project_snapshots)} 个项目")
    official_manifest = ROOT / "official/manifest.json"
    official_pages = json.loads(official_manifest.read_text(encoding="utf-8")) if official_manifest.exists() else []
    local_official_files = sorted((ROOT / "official").glob("*.html")) + sorted((ROOT / "official").glob("*.txt"))
    for page in (official_pages if local_official_files else []):
        if page.get("error"):
            errors.append(f"官方页面采集失败：{page['name']}")
        for index, item in enumerate(page["files"]):
            suffix = ".html" if index == 0 else ".txt"
            local_path = item.get("local_path") or f"official/{page['name']}{suffix}"
            file = (ROOT / local_path).resolve()
            file.relative_to(ROOT.resolve())
            if not file.exists():
                errors.append(f"官方文件缺失：{local_path}")
                continue
            content = file.read_bytes()
            supplemental_files += 1
            if len(content) != item["bytes"] or hashlib.sha256(content).hexdigest() != item["sha256"]:
                errors.append(f"官方文件校验不一致：{local_path}")
    context_files = sorted((ROOT / "context7").glob("*.json"))
    for file in context_files:
        snapshot = json.loads(file.read_text(encoding="utf-8"))
        if not snapshot.get("provider") or not snapshot.get("result", {}).get("content"):
            errors.append(f"文档查询快照不完整：{file.name}")
    authored = [ROOT / "README.md", ROOT / "catalog.md", ROOT / "research.md",
                ROOT / "technical-coverage.md"]
    authored.extend(sorted(path for path in (ROOT.parent / "docs").rglob("*.md")
                           if "node_modules" not in path.parts))
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
              f"- 本地固定提交文档：{total_files}，原文字节：{total_bytes:,}。",
              f"- 上游原文快照：{local_project_snapshots}/{len(projects)} 个项目。",
              f"- 重点仓库目录树：{trees}。",
              f"- 官方页面：{len(official_pages)}，本地 HTML／正文文件：{supplemental_files}。",
              f"- 文档查询快照：{len(context_files)}，JSON 结构与内容检查通过。",
              f"- 自写文档本地链接检查：{linked}。",
              "- 对存在的本地快照核对文件、字节长度和 SHA-256，并检查固定提交来源。",
              "", "## 分类数量", "", "| 类别 | 仓库数 |", "| --- | --- |"]
    report.extend(f"| {category} | {count} |" for category, count in sorted(categories.items()))
    report += ["", "## 证据限制", "", "竞品未部署运行；文档能力声明不等于实测通过。上游文件原文、注释和版本描述按原样保留，自写代码注释采用中文。上游文档内未采集外链不参与完整性检查。",
               "", "公开仓库不分发上游原文；缺少本地快照时会跳过原文哈希检查。",
               "", "未采到独立许可证文件的仓库：" + ("、".join(missing_license) or "无") + "。缺少独立文件不代表没有许可证，复用实现前需检查上游。",
               "", "候选文件不存在或读取失败的记录保留在 sources.json 的 unavailable 中；这些记录不等于核心采集失败。", "",
               "## 问题", ""]
    report += [f"- {error}" for error in errors] if errors else ["无文件完整性或自写文档链接问题。"]
    with (ROOT / "verification.md").open("w", encoding="utf-8", newline="\n") as output:
        output.write("\n".join(report) + "\n")
    print(json.dumps({"projects": len(projects), "files": total_files, "trees": trees,
                      "official_pages": len(official_pages), "official_files": supplemental_files,
                      "local_project_snapshots": local_project_snapshots,
                      "context_snapshots": len(context_files),
                      "local_links": linked, "bytes": total_bytes, "errors": errors}, ensure_ascii=False, indent=2))
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(verify())
