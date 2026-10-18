import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  AlertTriangle, ArrowDownToLine, ArrowLeft, ArrowUpDown, Check,
  CheckCircle2, ChevronDown, ChevronRight, Clock3, Copy, File,
  FileArchive, FileImage, FileSpreadsheet, FileText, Folder, FolderPlus,
  HardDrive, Image, List, Menu, Monitor, MoreHorizontal, Move, Play, Plus,
  Search, Settings2, ShieldCheck, Smartphone, Sparkles, Upload, X,
} from 'lucide-react';
import './styles.css';

const initialFiles = [
  { id: 'folder', name: '2025 年秋季照片', type: 'folder', path: '照片', size: '—', modified: '今天 10:42', detail: '包含 128 张照片和 6 段视频' },
  { id: 'photo', name: '海边日落.CR3', type: 'image', path: '照片 / 未整理', size: '28.4 MB', modified: '今天 09:16', detail: 'Canon RAW · 拍摄于 2025-10-14 17:42' },
  { id: 'pdf', name: '房屋租赁合同.pdf', type: 'pdf', path: '文档 / 合同', size: '2.1 MB', modified: '昨天 19:30', detail: 'PDF 文档 · 12 页' },
  { id: 'md', name: '产品需求_v3.md', type: 'text', path: '项目 / FileSage', size: '48 KB', modified: '昨天 15:08', detail: 'Markdown · 需求文档' },
  { id: 'sheet', name: '报销单_10月.xlsx', type: 'sheet', path: '文档 / 财务', size: '316 KB', modified: '10 月 12 日', detail: 'Excel 工作簿 · 3 个工作表' },
];

const iconFor = (type) => ({ folder: Folder, image: FileImage, pdf: FileText, text: FileText, sheet: FileSpreadsheet, archive: FileArchive }[type] || File);
const navItems = [
  { id: 'files', label: '文件', Icon: Folder },
  { id: 'photos', label: '照片', Icon: Image },
  { id: 'agent', label: 'Agent', Icon: Sparkles },
  { id: 'tasks', label: '任务', Icon: Clock3 },
];

function Button({ children, kind = '', onClick, disabled, title, type = 'button' }) {
  return <button className={`button ${kind}`} type={type} onClick={onClick} disabled={disabled} title={title}>{children}</button>;
}

function App() {
  const [page, setPage] = useState('files');
  const [mobilePreview, setMobilePreview] = useState(false);
  const workspaceRef = useRef(null);
  const [files] = useState(initialFiles);
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState([]);
  const [activeFile, setActiveFile] = useState(initialFiles[1]);
  const [detailOpen, setDetailOpen] = useState(false);
  const [toastText, setToastText] = useState('');
  const [moreOpen, setMoreOpen] = useState(false);
  const [preview, setPreview] = useState('list');
  const visibleFiles = useMemo(() => files.filter((file) => file.name.toLowerCase().includes(query.toLowerCase())), [files, query]);

  useEffect(() => {
    if (workspaceRef.current) workspaceRef.current.scrollTop = 0;
    if (window.innerWidth < 820) window.scrollTo(0, 0);
  }, [page, mobilePreview]);

  function toast(message) {
    setToastText(message);
    window.clearTimeout(window.toastTimer);
    window.toastTimer = window.setTimeout(() => setToastText(''), 2200);
  }

  function selectFile(file) {
    setActiveFile(file);
    if (mobilePreview || window.innerWidth < 820) setDetailOpen(true);
  }

  function toggleSelected(id) {
    setSelected((current) => current.includes(id) ? current.filter((value) => value !== id) : [...current, id]);
  }

  return (
    <div className={`app-shell ${mobilePreview ? 'mobile-preview' : ''} ${page === 'files' ? 'with-detail' : ''}`}>
      <aside className="sidebar">
        <a className="brand" href="#files" onClick={() => setPage('files')}>
          <span className="brand-mark"><Folder size={21} strokeWidth={1.8} /></span>
          <span><b>FileSage</b><small>个人文件工作台</small></span>
        </a>
        <div className="side-section-label">工作区</div>
        <nav className="side-nav" aria-label="主导航">
          {navItems.map(({ id, label, Icon }) => <button key={id} className={page === id ? 'active' : ''} onClick={() => { setPage(id); setDetailOpen(false); }}><Icon size={18} />{label}{id === 'agent' && <span className="nav-count">2</span>}</button>)}
        </nav>
        <div className="volume-card"><div className="volume-head"><HardDrive size={15} /><span>存储卷</span><span className="online-dot" /></div><b>3 个卷在线</b><div className="volume-bar"><i /></div><small>已用 2.4 TB / 8 TB</small></div>
        <button className="side-settings" onClick={() => setMoreOpen((value) => !value)}><Settings2 size={17} />设置与更多</button>
        {moreOpen && <div className="side-popover"><b>服务状态</b><span><i className="online-dot" /> 文件服务运行中</span><span>模型：本地服务</span><button onClick={() => { setMoreOpen(false); toast('设置页将在产品实现阶段接入'); }}>打开设置 <ChevronRight size={14} /></button></div>}
        <div className="side-foot">仅在本地网络内使用<br /><span>根目录状态正常</span></div>
      </aside>

      <main className="workspace" ref={workspaceRef}>
        <header className="topbar">
          <div className="mobile-brand"><button className="icon-button" aria-label="打开导航" onClick={() => toast('手机端使用底部导航')}><Menu size={19} /></button><span>FileSage</span></div>
          <div className="top-crumb"><span>个人空间</span><ChevronRight size={13} /><b>{pageLabel(page)}</b></div>
          <div className="top-actions">
            <div className="system-status"><i className="online-dot" />3 个卷在线</div>
            <button className="icon-button more-mobile" aria-label="更多" onClick={() => setMoreOpen((value) => !value)}><MoreHorizontal size={19} /></button>
            <div className="preview-switch" aria-label="原型视口预览"><button className={!mobilePreview ? 'chosen' : ''} onClick={() => setMobilePreview(false)} title="桌面预览"><Monitor size={15} /></button><button className={mobilePreview ? 'chosen' : ''} onClick={() => setMobilePreview(true)} title="手机预览"><Smartphone size={15} /></button></div>
          </div>
        </header>

        <div className="page-content">
          {page === 'files' && <FilesPage
            files={visibleFiles} query={query} setQuery={setQuery} selected={selected} toggleSelected={toggleSelected}
            activeFile={activeFile} selectFile={selectFile} preview={preview} setPreview={setPreview} toast={toast}
            openAgent={() => setPage('agent')}
          />}
          {page === 'photos' && <PhotosPage toast={toast} openAgent={() => setPage('agent')} />}
          {page === 'agent' && <AgentPage toast={toast} />}
          {page === 'tasks' && <TasksPage toast={toast} />}
        </div>
        <nav className="mobile-nav" aria-label="手机主导航">
          {navItems.map(({ id, label, Icon }) => <button key={id} className={page === id ? 'active' : ''} onClick={() => { setPage(id); setDetailOpen(false); }}><Icon size={19} strokeWidth={page === id ? 2.2 : 1.8} /><span>{label}</span>{id === 'agent' && <i className="mobile-badge">2</i>}</button>)}
        </nav>
      </main>

      {page === 'files' && <aside className={`detail-panel ${detailOpen ? 'open' : ''}`}>
        <div className="detail-top"><span>文件详情</span><button className="icon-button" aria-label="关闭详情" onClick={() => setDetailOpen(false)}><X size={17} /></button></div>
        <div className="file-preview"><FilePreview file={activeFile} /></div>
        <div className="detail-name"><div className="file-type-icon"><IconFor file={activeFile} /></div><div><b>{activeFile.name}</b><span>{activeFile.detail}</span></div></div>
        <div className="detail-path"><small>所在位置</small><span>{activeFile.path}</span></div>
        <div className="metadata-list"><span>大小 <b>{activeFile.size}</b></span><span>修改时间 <b>{activeFile.modified}</b></span><span>状态 <b className="status-plain">可读取</b></span></div>
        <Button kind="outline full" onClick={() => toast('原型：打开阅读器')}>打开预览 <ChevronRight size={15} /></Button>
      </aside>}

      {toastText && <div className="toast" role="status"><CheckCircle2 size={16} />{toastText}</div>}
    </div>
  );
}

function pageLabel(page) { return ({ files: '文件', photos: '照片', agent: '文件整理', tasks: '任务' })[page]; }
function IconFor({ file }) { const Icon = iconFor(file.type); return <Icon size={19} strokeWidth={1.8} />; }

function FilesPage({ files, query, setQuery, selected, toggleSelected, activeFile, selectFile, preview, setPreview, toast, openAgent }) {
  return <>
    <div className="page-heading"><div><div className="eyebrow">管理根目录 · 3 个卷在线</div><h1>我的文件</h1><p>通过真实文件夹整理和查找内容</p></div><div className="heading-actions"><Button kind="outline" onClick={() => toast('原型：选择上传文件')}><Upload size={16} />上传</Button><Button kind="primary" onClick={() => toast('原型：新建文件夹')}><FolderPlus size={16} />新建文件夹</Button></div></div>
    <div className="breadcrumbs"><span>全部文件</span><ChevronRight size={13} /><span>文档</span><ChevronRight size={13} /><b>近期整理</b><button className="breadcrumb-more" aria-label="目录操作" onClick={() => toast('原型：目录操作菜单')}><MoreHorizontal size={17} /></button></div>
    <div className="file-toolbar"><label className="search-field"><Search size={16} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索当前目录" /></label><div className="filter-buttons"><button onClick={() => toast('原型：筛选类型')}><span>类型</span><ChevronDown size={13} /></button><button onClick={() => toast('原型：筛选修改时间')}><span>修改时间</span><ChevronDown size={13} /></button></div><div className="view-switch"><button aria-label="列表视图" className={preview === 'list' ? 'active' : ''} onClick={() => setPreview('list')}><List size={16} /></button><button aria-label="缩略图视图" className={preview === 'grid' ? 'active' : ''} onClick={() => setPreview('grid')}><Image size={16} /></button></div></div>
    {selected.length > 0 && <div className="selection-bar"><b>已选择 {selected.length} 项</b><button onClick={() => toast('原型：移动所选文件')}><Move size={15} />移动</button><button onClick={() => toast('原型：复制所选文件')}><Copy size={15} />复制</button><button className="clear-select" onClick={() => toggleSelected(selected[0])}>取消选择</button></div>}
    {preview === 'list' ? <div className="file-list"><div className="list-head"><span>名称</span><span>大小</span><span>修改时间</span><span /></div>{files.map((file) => <FileRow key={file.id} file={file} checked={selected.includes(file.id)} active={activeFile.id === file.id} onCheck={() => toggleSelected(file.id)} onClick={() => selectFile(file)} toast={toast} />)}{files.length === 0 && <div className="empty-state"><Search size={22} /><b>没有找到匹配文件</b><span>尝试缩短关键词或清空搜索条件。</span></div>}</div> : <div className="file-grid">{files.map((file) => <button key={file.id} className={`file-grid-card ${activeFile.id === file.id ? 'selected' : ''}`} onClick={() => selectFile(file)}><div className={`grid-art art-${file.type}`}><IconFor file={file} /></div><span>{file.name}</span><small>{file.size} · {file.modified}</small></button>)}</div>}
    <div className="list-footer"><span>显示 {files.length} 项</span><span>文件系统为准 · <button onClick={() => toast('已刷新目录')}>刷新</button></span></div>
    <button className="agent-quick" onClick={openAgent}><Sparkles size={16} /><span>让 Agent 帮我整理</span><small>先分析，审批后执行</small><ChevronRight size={16} /></button>
  </>;
}

function FileRow({ file, checked, active, onCheck, onClick, toast }) {
  return <div className={`file-row ${active ? 'active' : ''} ${checked ? 'checked' : ''}`}>
    <label className="row-check"><input type="checkbox" checked={checked} onChange={onCheck} aria-label={`选择 ${file.name}`} /></label>
    <button className="file-name-cell" onClick={onClick}><span className={`row-icon type-${file.type}`}><IconFor file={file} /></span><span className="row-name-wrap"><b>{file.name}</b><small>{file.path}</small></span></button>
    <span className="row-size">{file.size}</span><span className="row-date">{file.modified}</span><button className="row-menu" aria-label={`${file.name} 的更多操作`} onClick={() => toast(`原型：${file.name} 操作菜单`)}><MoreHorizontal size={17} /></button>
  </div>;
}

function FilePreview({ file }) {
  if (file.type === 'image') return <div className="preview-photo"><div className="sunset-sun" /><div className="sunset-hill" /><div className="sunset-water" /></div>;
  if (file.type === 'pdf') return <div className="preview-document"><div className="doc-logo">契</div><i /><i /><i className="short" /><i /><i /><span>房屋租赁合同</span></div>;
  if (file.type === 'sheet') return <div className="preview-sheet"><div /><div /><div /><div /><div /><div /><div /><div /><div /></div>;
  if (file.type === 'folder') return <div className="preview-folder"><Folder size={58} strokeWidth={1.1} /></div>;
  return <div className="preview-text"><span># 产品需求</span><i>面向个人使用的文件管理系统。</i><i>原生文件夹作为分类方式。</i><i>Agent 操作需要逐项审批。</i></div>;
}

function PhotosPage({ toast, openAgent }) {
  const photos = [
    ['海边日落', '17:42', 'sunset'], ['山间步道', '14:08', 'forest'], ['营地清晨', '07:31', 'lake'], ['城市夜景', '20:14', 'city'], ['沿途风景', '16:06', 'coast'], ['云层间隙', '18:03', 'cloud'],
  ];
  return <>
    <div className="page-heading"><div><div className="eyebrow">基于真实照片文件</div><h1>照片</h1><p>按拍摄时间浏览，文件仍保存在原目录</p></div><div className="heading-actions"><Button kind="outline" onClick={() => toast('原型：照片筛选')}><ArrowUpDown size={15} />筛选</Button><Button kind="primary" onClick={openAgent}><Sparkles size={16} />按日期归档</Button></div></div>
    <div className="photo-date-head"><b>2025 年 10 月 14 日</b><span>6 张照片 · 2 个目录</span></div><div className="photo-grid">{photos.map(([name, time, style], index) => <button className="photo-card" key={name} onClick={() => toast(`原型：打开照片 ${name}`)}><div className={`photo-art photo-${style}`}><span className="photo-index">{String(index + 1).padStart(2, '0')}</span></div><b>{name}</b><small>{time} · {index % 2 ? '相机' : '手机'}</small></button>)}</div>
    <div className="photo-note"><Image size={16} /><span>照片浏览按可读拍摄日期排序；归档操作会先由 Agent 列出移动清单。</span></div>
  </>;
}

// Agent 原型展示对话确认、逐项审核和二次执行确认。
function AgentPage({ toast }) {
  const [stage, setStage] = useState('confirm');
  const [completed, setCompleted] = useState(false);
  const [decision, setDecision] = useState('pending');

  function startNewConversation() {
    setStage('confirm');
    setCompleted(false);
    setDecision('pending');
  }

  return <>
    <div className="page-heading"><div><div className="eyebrow">对话 · 分析 · 审核</div><h1>文件整理</h1><p>先说清目标，再审核 Agent 提出的操作</p></div><Button kind="outline" onClick={startNewConversation}>新对话</Button></div>
    <section className="agent-chat-panel" aria-label="Agent 整理对话">
      <div className="agent-chat-heading"><span className="agent-chat-icon"><Sparkles size={17} /></span><div><b>和 Agent 对话</b><small>{completed ? '已完成' : stage === 'confirm' ? '待你确认分析' : '待你审核方案'}</small></div><span className="review-pill"><ShieldCheck size={15} />{completed ? '已完成' : stage === 'confirm' ? '待确认' : decision === 'approved' ? '已批准' : decision === 'rejected' ? '已拒绝' : '待审核'}</span></div>
      <div className="agent-chat-thread">
        <article className="agent-message"><span className="agent-message-icon"><Sparkles size={14} /></span><div><b>Agent</b><p>我会先和你确认整理目标与文件范围；分析后只提交操作方案，审批通过并再次确认后才会修改文件。</p></div></article>
        <article className="agent-message is-user"><span className="agent-message-icon">你</span><div><b>你</b><p>请根据正文标题重命名这份合同，保留原目录。</p></div></article>
        <article className="agent-message"><span className="agent-message-icon"><Sparkles size={14} /></span><div><b>Agent</b><p>我理解为：读取合同正文，为这一个文件提出同目录的新名称。确认后开始分析，可以吗？</p></div></article>
      </div>
      {stage === 'confirm' && !completed && <section className="agent-confirm-card" aria-labelledby="agent-confirm-title">
        <h2 id="agent-confirm-title"><ShieldCheck size={16} />确认分析范围</h2>
        <p className="agent-confirm-goal">根据正文标题生成同目录重命名建议</p>
        <dl><div><dt>文件</dt><dd><code>文档/待整理/合同.md</code></dd></div><div><dt>发送内容</dt><dd>文件正文</dd></div><div><dt>模型</dt><dd>本地模型 · qwen3:8b</dd></div></dl>
        <p className="agent-confirm-note">确认后生成方案，不会直接修改文件。</p>
        <Button kind="primary" onClick={() => { setStage('review'); toast('原型：已生成待审核方案'); }}><Check size={15} />确认分析</Button>
      </section>}
      {stage === 'review' && !completed && <section className="agent-review-card" aria-labelledby="agent-review-title">
        <div className="agent-review-heading"><div><h2 id="agent-review-title">方案审核</h2><p>核对操作和文件路径；批准后仍需确认执行。</p></div><span>1 项待审核</span></div>
        <article className={`approval-card ${decision === 'approved' ? 'approved' : ''}`}><div className="approval-body"><div className="approval-title"><span className="approval-icon"><FileText size={17} /></span><b>重命名合同.md</b><span className={`agent-item-status${decision === 'approved' ? ' is-approved' : decision === 'rejected' ? ' is-rejected' : ''}`}>{decision === 'approved' ? '已批准' : decision === 'rejected' ? '已拒绝' : '待审核'}</span></div><div className="path-change"><span>文档/待整理/合同.md</span><ChevronRight size={14} /><b>文档/待整理/供应商合同.md</b></div><p>依据正文标题拟定新名称。</p><div className="agent-item-actions">{decision === 'pending' ? <><Button kind="primary" onClick={() => { setDecision('approved'); toast('原型：已批准本项'); }}><Check size={15} />批准</Button><Button kind="outline" onClick={() => { setDecision('rejected'); toast('原型：已拒绝本项'); }}>拒绝</Button></> : <Button kind="outline" onClick={() => setDecision('pending')}>重新审核</Button>}</div></div></article>
        <div className="agent-review-footer"><span>本项状态：<b>{decision === 'approved' ? '已批准' : decision === 'rejected' ? '已拒绝' : '待审核'}</b></span></div>
        {decision === 'approved' && <div className="agent-execute"><span>执行前再确认一次。</span><Button kind="primary" onClick={() => { setCompleted(true); toast('原型：执行完成'); }}><CheckCircle2 size={15} />确认执行</Button></div>}
      </section>}
      {completed && <div className="agent-complete"><CheckCircle2 size={17} /><span>操作已完成：合同.md 已重命名为供应商合同.md。</span></div>}
      <form className="agent-composer" onSubmit={(event) => { event.preventDefault(); toast('原型：消息发送交互'); }}><textarea aria-label="继续和 Agent 对话" placeholder="继续和 Agent 对话，补充目标或范围…" rows={2} /><div><span>操作前会先向你确认</span><Button kind="primary" type="submit"><Sparkles size={15} />发送</Button></div></form>
    </section>
  </>;
}

function TasksPage({ toast }) {
  return <>
    <div className="page-heading"><div><div className="eyebrow">上传、处理与 Agent 历史</div><h1>任务</h1><p>查看运行状态、部分结果和失败原因</p></div><Button kind="outline" onClick={() => toast('任务列表已刷新')}><Clock3 size={15} />刷新</Button></div>
    <div className="task-tabs"><button className="active">全部 <span>4</span></button><button>进行中 <span>1</span></button><button>已完成 <span>2</span></button><button>失败 <span>1</span></button></div>
    <div className="task-list">
      <article className="task-item"><div className="task-icon upload-task"><Upload size={17} /></div><div className="task-main"><div className="task-title"><b>上传 24 个文件</b><span className="task-state running">上传中</span></div><span className="task-path">/个人空间/照片/2025 年秋季照片</span><div className="progress-track"><i /></div><div className="task-sub"><span>18 / 24 个文件 · 2.8 GB / 3.6 GB</span><b>78%</b></div></div><button className="icon-button" aria-label="暂停上传" onClick={() => toast('原型：暂停上传')}><span className="pause-icon">Ⅱ</span></button></article>
      <article className="task-item"><div className="task-icon agent-task"><Sparkles size={17} /></div><div className="task-main"><div className="task-title"><b>照片与文档整理</b><span className="task-state waiting">待审批</span></div><span className="task-path">3 项建议 · 2 项可恢复，1 项永久删除</span><button className="task-link" onClick={() => toast('请使用 Agent 页面查看审批清单')}>继续审批 <ChevronRight size={13} /></button></div><button className="icon-button" aria-label="打开任务详情" onClick={() => toast('原型：任务详情')}><MoreHorizontal size={17} /></button></article>
      <article className="task-item"><div className="task-icon done-task"><CheckCircle2 size={17} /></div><div className="task-main"><div className="task-title"><b>建立照片缩略图</b><span className="task-state done">已完成</span></div><span className="task-path">/照片 · 处理 384 个文件 · 今天 10:20</span></div><button className="icon-button" aria-label="打开任务详情" onClick={() => toast('原型：任务详情')}><MoreHorizontal size={17} /></button></article>
      <article className="task-item"><div className="task-icon warning-task"><AlertTriangle size={17} /></div><div className="task-main"><div className="task-title"><b>解压 archive_2024.zip</b><span className="task-state failed">部分失败</span></div><span className="task-path">8 项成功 · 2 项因目标目录权限不足未解压</span><button className="task-link" onClick={() => toast('原型：查看失败项与重试')}>查看失败项 <ChevronRight size={13} /></button></div><button className="icon-button" aria-label="重试解压" onClick={() => toast('原型：重试失败项')}><Play size={15} /></button></article>
    </div>
  </>;
}

function createRootIfReady() { createRoot(document.getElementById('root')).render(<App />); }
createRootIfReady();
