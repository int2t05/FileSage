"""Build the local, self-contained icon study page from pinned Lucide SVGs."""

import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT / "projects/frontend-design/lucide-icons__lucide/upstream/icons"
OUTPUT = ROOT.parent / "docs/design/icon-study.html"

GROUPS = [
    ("导航", "navigation", [
        ("folder", "文件", "常用导航"), ("folder-open", "打开目录", "常用导航"),
        ("image", "照片", "常用导航"), ("clock-3", "任务", "常用导航"),
        ("search", "搜索", "常用导航"), ("settings-2", "设置", "常用导航"),
    ]),
    ("文件与磁盘", "files", [
        ("file", "文件", "文件与磁盘"), ("file-text", "文本文档", "文件与磁盘"),
        ("file-image", "图片文件", "文件与磁盘"), ("file-archive", "压缩包", "文件与磁盘"),
        ("hard-drive", "磁盘卷", "文件与磁盘"), ("grid-2x2", "缩略图", "文件与磁盘"),
    ]),
    ("文件操作", "actions", [
        ("upload", "上传", "文件操作"), ("download", "下载", "文件操作"),
        ("copy", "复制", "文件操作"), ("move", "移动", "文件操作"),
        ("pencil", "重命名／编辑", "文件操作"), ("arrow-up-down", "排序", "文件操作"),
    ]),
    ("状态与阅读", "states", [
        ("check", "已完成", "状态与阅读"), ("triangle-alert", "需要注意", "状态与阅读"),
        ("shield-check", "审批通过", "状态与阅读"), ("refresh-cw", "重新加载", "状态与阅读"),
        ("list", "列表视图", "状态与阅读"), ("play", "播放", "状态与阅读"),
        ("sun", "浅色主题", "状态与阅读"), ("moon", "深色主题", "状态与阅读"),
    ]),
]


def icon_markup(name):
    text = (REPO / f"{name}.svg").read_text(encoding="utf-8")
    body = re.search(r"<svg\b[^>]*>(.*?)</svg>", text, re.S).group(1)
    return f'<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false">{body.strip()}</svg>'


def main():
    cards = []
    for heading, category, icons in GROUPS:
        for name, label, group in icons:
            cards.append({"name": name, "label": label, "group": group,
                          "category": category, "svg": icon_markup(name)})
    payload = json.dumps(cards, ensure_ascii=False).replace("</", "<\\/")
    page = r'''<!doctype html>
<html lang="zh-CN" data-theme="light">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="description" content="FileSage 图标系统审计页：基于固定提交的 Lucide 图标样本。">
<title>FileSage · 图标规范</title>
<style>
:root{font-family:Inter,"Noto Sans SC","Microsoft YaHei",system-ui,sans-serif;color:#222a32;background:#f4f6f8;font-synthesis:none;text-rendering:optimizeLegibility;--bg:#f4f6f8;--surface:#fff;--surface2:#f8f9fa;--text:#222a32;--muted:#727b85;--line:#e7eaed;--accent:#176b5b;--accent-soft:#e6f3ef;--danger:#bd4c45;--shadow:0 12px 40px #2432420a;--icon-size:25px}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text)}button,input{font:inherit}button{cursor:pointer}svg{width:var(--icon-size);height:var(--icon-size);fill:none;stroke:currentColor;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round;flex:none}.app{min-height:100vh;display:grid;grid-template-columns:232px minmax(0,1fr)}.sidebar{background:#19232b;color:#e9eff2;padding:25px 16px;display:flex;flex-direction:column;gap:31px}.brand{display:flex;gap:11px;align-items:center;padding:0 10px}.brand-mark{height:34px;width:34px;border-radius:10px;background:#d8eee7;color:#176b5b;display:grid;place-items:center}.brand-mark svg{width:21px;height:21px}.brand-name{font-weight:700;font-size:15px;letter-spacing:.01em}.brand-sub{color:#a9b8bf;font-size:11px;margin-top:2px}.nav-label{font-size:10px;letter-spacing:.12em;text-transform:uppercase;color:#82939b;padding:0 12px;margin:4px 0 9px}.nav{display:grid;gap:4px}.nav a{display:flex;align-items:center;gap:12px;text-decoration:none;color:#b8c5ca;padding:10px 12px;border-radius:8px;font-size:13px}.nav a svg{width:18px;height:18px}.nav a.active{color:#f2fcf9;background:#29443f}.nav a.active svg{color:#a9e0d0}.side-note{margin-top:auto;border:1px solid #34434a;border-radius:10px;padding:13px;color:#b8c5ca;font-size:11px;line-height:1.65}.side-note strong{display:block;color:#edf4f5;font-size:12px;margin-bottom:4px}.main{min-width:0}.topbar{height:66px;border-bottom:1px solid var(--line);background:var(--surface);display:flex;align-items:center;justify-content:space-between;padding:0 32px}.crumb{font-size:12px;color:var(--muted)}.crumb b{color:var(--text);font-weight:600}.top-actions{display:flex;align-items:center;gap:10px}.pill{font-size:11px;background:var(--accent-soft);color:var(--accent);border-radius:99px;padding:6px 10px;font-weight:600}.icon-button{height:34px;width:34px;border:1px solid var(--line);background:var(--surface);border-radius:8px;color:var(--text);display:grid;place-items:center}.icon-button svg{width:17px;height:17px}.content{max-width:1280px;margin:0 auto;padding:34px 38px 52px}.eyebrow{color:var(--accent);font-size:11px;font-weight:700;letter-spacing:.09em;text-transform:uppercase}.heading{display:flex;justify-content:space-between;align-items:flex-end;gap:20px;margin:8px 0 24px}.heading h1{font-size:27px;letter-spacing:-.035em;margin:0;font-weight:680}.heading p{margin:8px 0 0;color:var(--muted);font-size:13px}.commit{font-size:10px;color:var(--muted);padding:8px 11px;border:1px solid var(--line);border-radius:7px;background:var(--surface);white-space:nowrap}.summary{display:grid;grid-template-columns:1.6fr repeat(3,1fr);gap:12px;margin-bottom:22px}.summary-card{background:var(--surface);border:1px solid var(--line);border-radius:11px;padding:15px 17px;box-shadow:var(--shadow)}.summary-card.primary{background:#eaf4f1;border-color:#d8eae4}.summary-label{font-size:11px;color:var(--muted)}.summary-value{font-size:21px;letter-spacing:-.03em;font-weight:660;margin-top:8px}.summary-value small{font-size:11px;color:var(--muted);font-weight:500;letter-spacing:0}.summary-note{font-size:10px;color:var(--muted);margin-top:5px}.toolbar{display:flex;align-items:center;gap:10px;background:var(--surface);border:1px solid var(--line);padding:10px;border-radius:11px;margin-bottom:24px}.search{display:flex;align-items:center;gap:8px;flex:1;max-width:340px;height:36px;border:1px solid var(--line);border-radius:7px;padding:0 10px;color:var(--muted)}.search svg{width:16px;height:16px}.search input{background:transparent;border:0;outline:0;color:var(--text);width:100%;font-size:12px}.filters{display:flex;gap:5px;flex-wrap:wrap}.filter{height:32px;border:0;background:transparent;border-radius:6px;padding:0 10px;color:var(--muted);font-size:11px}.filter.active{background:var(--accent-soft);color:var(--accent);font-weight:650}.toolbar-spacer{flex:1}.size-control{display:flex;align-items:center;gap:7px;color:var(--muted);font-size:10px}.size-control input{width:78px;accent-color:var(--accent)}.section{margin:27px 0 0}.section-head{display:flex;align-items:center;justify-content:space-between;margin-bottom:10px}.section-head h2{font-size:13px;margin:0;font-weight:650}.section-head span{font-size:10px;color:var(--muted)}.grid{display:grid;grid-template-columns:repeat(6,minmax(0,1fr));gap:9px}.icon-card{min-width:0;position:relative;background:var(--surface);border:1px solid var(--line);border-radius:9px;padding:13px 12px 11px;transition:transform .16s,border-color .16s,box-shadow .16s}.icon-card:hover{transform:translateY(-2px);border-color:#b8d8ce;box-shadow:0 6px 18px #263b340d}.icon-card .preview{height:48px;display:flex;align-items:center;justify-content:center;color:#34434a;margin-bottom:8px}.icon-card .preview svg{width:var(--icon-size);height:var(--icon-size)}.icon-card .label{font-size:11px;color:var(--text);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.icon-card .name{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;color:var(--muted);font-size:9px;margin-top:4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.copy{position:absolute;top:8px;right:8px;opacity:0;border:1px solid var(--line);background:var(--surface);border-radius:5px;color:var(--muted);font-size:9px;padding:4px 5px}.icon-card:hover .copy,.copy:focus{opacity:1}.compare{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin-top:30px}.compare-card{border:1px solid var(--line);background:var(--surface);border-radius:10px;padding:15px}.compare-card h3{margin:0;font-size:12px}.compare-card p{font-size:10px;line-height:1.6;color:var(--muted);margin:7px 0 0}.compare-card .tag{font-size:9px;color:var(--accent);font-weight:650;margin-top:9px}.note{margin-top:18px;border-left:3px solid #d9a653;background:#fff9ec;color:#685832;padding:11px 14px;font-size:11px;line-height:1.6;border-radius:0 7px 7px 0}.footer{margin-top:24px;color:var(--muted);font-size:10px;line-height:1.7}.footer a{color:var(--accent)}.empty{grid-column:1/-1;padding:30px;text-align:center;color:var(--muted);font-size:12px;border:1px dashed var(--line);border-radius:8px}
html[data-theme="dark"]{--bg:#11181d;--surface:#192329;--surface2:#202b31;--text:#e7edef;--muted:#a2afb4;--line:#2d3b41;--accent:#8bd4bd;--accent-soft:#243e38;--shadow:0 12px 40px #0003}html[data-theme="dark"] .sidebar{background:#0b1115}html[data-theme="dark"] .summary-card.primary{background:#203a34;border-color:#31534a}html[data-theme="dark"] .icon-card .preview{color:#e0e9e9}html[data-theme="dark"] .note{background:#342e22;color:#e8d7ad}
@media(max-width:1100px){.grid{grid-template-columns:repeat(4,minmax(0,1fr))}.summary{grid-template-columns:repeat(4,1fr)}.summary-card.primary{grid-column:span 2}}@media(max-width:820px){.app{grid-template-columns:1fr}.sidebar{display:none}.topbar{padding:0 18px;height:58px}.content{padding:25px 18px 40px}.heading{align-items:flex-start;flex-direction:column}.summary{grid-template-columns:repeat(2,1fr)}.summary-card.primary{grid-column:span 2}.toolbar{flex-wrap:wrap}.search{max-width:none;flex-basis:100%}.toolbar-spacer{display:none}.grid{grid-template-columns:repeat(3,minmax(0,1fr))}.compare{grid-template-columns:1fr}.commit{white-space:normal}}@media(max-width:440px){.content{padding-left:13px;padding-right:13px}.topbar{padding:0 13px}.pill{display:none}.grid{grid-template-columns:repeat(2,minmax(0,1fr))}.heading h1{font-size:23px}.size-control{margin-left:auto}}
</style></head><body>
<div class="app"><aside class="sidebar"><div class="brand"><div class="brand-mark" id="brandIcon"></div><div><div class="brand-name">FileSage</div><div class="brand-sub">个人文件工作台</div></div></div><div><div class="nav-label">工作区</div><nav class="nav"><a href="#"><span data-icon="folder"></span>文件</a><a href="#"><span data-icon="image"></span>照片</a><a href="#"><span data-icon="clock-3"></span>任务</a><a href="#"><span data-icon="shield-check"></span>Agent 审批</a></nav></div><div><div class="nav-label">设计系统</div><nav class="nav"><a class="active" href="#icons"><span data-icon="grid-2x2"></span>图标库</a></nav></div><div class="side-note"><strong>图标原则</strong>统一线宽与尺寸。图标负责快速识别，操作文字负责说明意图。</div></aside>
<main class="main"><header class="topbar"><div class="crumb">设计系统&nbsp; / &nbsp;<b>图标规范</b></div><div class="top-actions"><span class="pill">已选定 · 设计基线</span><button class="icon-button" id="themeBtn" aria-label="切换浅色或深色预览" title="切换主题"></button></div></header>
<div class="content"><div class="eyebrow">Visual language / 01</div><div class="heading"><div><h1>图标规范</h1><p>为目录、文件动作和任务状态建立一致、可识别的图形语言。</p></div><div class="commit">Lucide · pinned · d672fc43</div></div>
<section class="summary"><div class="summary-card primary"><div class="summary-label">已选图标库</div><div class="summary-value">Lucide <small>线性图标系统</small></div><div class="summary-note">MIT · 24 × 24 网格 · 默认线宽 2</div></div><div class="summary-card"><div class="summary-label">本页样例</div><div class="summary-value">26 <small>枚图标</small></div><div class="summary-note">来自固定提交的 SVG 原稿</div></div><div class="summary-card"><div class="summary-label">界面默认尺寸</div><div class="summary-value" id="sizeReadout">24 <small>px</small></div><div class="summary-note">导航及列表动作建议值</div></div><div class="summary-card"><div class="summary-label">语义要求</div><div class="summary-value">图标 + 文案</div><div class="summary-note">危险操作不得只显示图形</div></div></section>
<div class="toolbar"><label class="search"><span data-icon="search"></span><input id="search" type="search" placeholder="搜索图标名称或用途…" aria-label="搜索图标"></label><div class="filters"><button class="filter active" data-filter="all">全部</button><button class="filter" data-filter="navigation">导航</button><button class="filter" data-filter="files">文件</button><button class="filter" data-filter="actions">操作</button><button class="filter" data-filter="states">状态与阅读</button></div><div class="toolbar-spacer"></div><label class="size-control">尺寸 <input id="size" type="range" min="18" max="32" value="24" aria-label="调整图标预览尺寸"><span id="sizeText">24</span></label></div>
<div id="gallery"></div><div class="compare"><article class="compare-card"><h3>Lucide · 已采用</h3><p>轻量线框感适合高密度文件操作。名称清晰、跨框架包可选；本页展示固定提交 SVG 样本。</p><div class="tag">FileSage 首期主图标库</div></article><article class="compare-card"><h3>Tabler Icons · 备选</h3><p>同为 24 单位网格的线性体系，许可页面注明 MIT；保持统一主库，只有覆盖明显不足时才重新评估。</p><div class="tag">作为整体替换评估</div></article><article class="compare-card"><h3>Phosphor · 备选</h3><p>提供多种视觉粗细，表达层次灵活；若重新评估，需统一字重以保持语义一致。</p><div class="tag">不与主库混用</div></article></div><div class="note"><b>交互规则：</b>状态同时使用文字或形状区分；图标按钮提供中文可访问名称；永久删除、覆盖和权限变更始终显示明确文字与影响范围。</div><div class="footer">样例取自 <a href="https://github.com/lucide-icons/lucide/tree/d672fc43dbf60962d9d829e35bc3b3e5ed89a178/icons">Lucide 固定提交 SVG</a>，本地原稿见 reference/projects/frontend-design/lucide-icons__lucide/upstream/icons/；上游许可证随来源一并保留。本页展示已选 Lucide 图标库的样例，暗色仅用于图标对比，不属于首期产品主题。<br>参考：<a href="https://lucide.dev/guide/">Lucide 指南</a> · <a href="https://tabler.io/license">Tabler 许可</a> · <a href="https://phosphoricons.com/">Phosphor</a></div></div></main></div><div id="toast" role="status" aria-live="polite" style="position:fixed;bottom:20px;left:50%;transform:translateX(-50%);background:#19232b;color:white;border-radius:7px;padding:9px 13px;font-size:11px;opacity:0;pointer-events:none;transition:opacity .2s"></div>
<script>
const icons=__ICON_DATA__;
const byName=Object.fromEntries(icons.map(i=>[i.name,i.svg]));
function setIcon(el,name){el.innerHTML=byName[name]||''}
document.querySelectorAll('[data-icon]').forEach(el=>setIcon(el,el.dataset.icon));
setIcon(document.getElementById('brandIcon'),'folder');setIcon(document.getElementById('themeBtn'),'sun');
const gallery=document.getElementById('gallery'), search=document.getElementById('search');let active='all';
function render(){const query=search.value.trim().toLowerCase();gallery.innerHTML='';let count=0;for(const {title,category} of groups){const selected=icons.filter(i=>i.category===category&&(active==='all'||active===category)&&(!query||(i.name+' '+i.label+' '+i.group).toLowerCase().includes(query)));if(!selected.length)continue;count+=selected.length;const section=document.createElement('section');section.className='section';section.innerHTML='<div class="section-head"><h2>'+title+'</h2><span>'+selected.length+' 个图标</span></div><div class="grid">'+selected.map(i=>'<article class="icon-card"><button class="copy" data-name="'+i.name+'" title="复制图标名称">复制名称</button><div class="preview">'+i.svg+'</div><div class="label">'+i.label+'</div><div class="name">'+i.name+'</div></article>').join('')+'</div>';gallery.append(section)}if(!count)gallery.innerHTML='<div class="empty">没有匹配的图标，试试其他名称。</div>';gallery.querySelectorAll('.copy').forEach(btn=>btn.addEventListener('click',async()=>{try{await navigator.clipboard.writeText(btn.dataset.name);toast('已复制：'+btn.dataset.name)}catch{toast('图标名称：'+btn.dataset.name)}}))}
const groups=__GROUP_DATA__;
function renderPage(){render()}
document.querySelectorAll('.filter').forEach(btn=>btn.addEventListener('click',()=>{document.querySelectorAll('.filter').forEach(b=>b.classList.remove('active'));btn.classList.add('active');active=btn.dataset.filter;renderPage()}));search.addEventListener('input',renderPage);
document.getElementById('size').addEventListener('input',e=>{document.documentElement.style.setProperty('--icon-size',e.target.value+'px');document.getElementById('sizeText').textContent=e.target.value;document.getElementById('sizeReadout').innerHTML=e.target.value+' <small>px</small>'});
document.getElementById('themeBtn').addEventListener('click',()=>{let dark=document.documentElement.dataset.theme!=='dark';document.documentElement.dataset.theme=dark?'dark':'light';setIcon(document.getElementById('themeBtn'),dark?'moon':'sun')});
let timer;function toast(message){let t=document.getElementById('toast');t.textContent=message;t.style.opacity=1;clearTimeout(timer);timer=setTimeout(()=>t.style.opacity=0,1500)}
renderPage();
</script></body></html>'''
    page = page.replace("__ICON_DATA__", payload)
    groups = [{"title": title, "category": category} for title, category, _ in GROUPS]
    page = page.replace("__GROUP_DATA__", json.dumps(groups, ensure_ascii=False))
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    OUTPUT.write_text(page, encoding="utf-8")
    print(f"Built {OUTPUT} with {len(cards)} source SVGs")


if __name__ == "__main__":
    main()
