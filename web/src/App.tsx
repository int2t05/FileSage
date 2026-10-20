import { lazy, Suspense, useEffect, useState } from 'react';
import { Clock3, Folder, HardDrive, Image, Settings2, Sparkles } from 'lucide-react';
import type { AgentLaunchContext } from './features/agent/AgentPage';
import './styles/navigation.css';

const AgentPage = lazy(() => import('./features/agent/AgentPage').then((module) => ({ default: module.AgentPage })));
const FilesPage = lazy(() => import('./features/files/FilesPage').then((module) => ({ default: module.FilesPage })));
const PhotosPage = lazy(() => import('./features/photos/PhotosPage').then((module) => ({ default: module.PhotosPage })));
const TrashPage = lazy(() => import('./features/trash/TrashPage').then((module) => ({ default: module.TrashPage })));
const TasksPage = lazy(() => import('./features/tasks/TasksPage').then((module) => ({ default: module.TasksPage })));
const AgentSettingsPage = lazy(() => import('./features/settings/AgentSettingsPage').then((module) => ({ default: module.AgentSettingsPage })));

type PageId = 'files' | 'photos' | 'trash' | 'agent' | 'tasks' | 'settings';

const navigation: { id: PageId; label: string; Icon: typeof Folder }[] = [
  { id: 'files', label: '文件', Icon: Folder },
  { id: 'photos', label: '照片', Icon: Image },
  { id: 'agent', label: 'Agent', Icon: Sparkles },
  { id: 'tasks', label: '任务', Icon: Clock3 },
];

const pageTitles: Record<PageId, string> = {
  files: '文件',
  photos: '照片',
  trash: '回收站',
  agent: '文件整理',
  tasks: '任务',
  settings: 'Agent 设置',
};

// pageFromHash 返回地址栏中支持的页面。
function pageFromHash(): PageId {
  const candidate = window.location.hash.slice(1) as PageId;
  return Object.prototype.hasOwnProperty.call(pageTitles, candidate) ? candidate : 'files';
}

export default function App() {
  const [page, setPage] = useState<PageId>(pageFromHash);
  const [serviceOnline, setServiceOnline] = useState(false);
  const [agentLaunchContext, setAgentLaunchContext] = useState<AgentLaunchContext | null>(null);

  // navigateTo 同步页面切换与地址栏。
  function navigateTo(next: PageId) {
    if (next !== 'agent') setAgentLaunchContext(null);
    if (window.location.hash !== `#${next}`) window.location.hash = next;
    setPage(next);
  }

  useEffect(() => {
    const syncPage = () => {
      const next = pageFromHash();
      if (next !== 'agent') setAgentLaunchContext(null);
      setPage(next);
    };
    window.addEventListener('hashchange', syncPage);
    return () => window.removeEventListener('hashchange', syncPage);
  }, []);

  useEffect(() => {
    let active = true;
    const checkHealth = () => {
      fetch('/api/v1/health')
        .then((response) => {
          if (active) setServiceOnline(response.ok);
        })
        .catch(() => {
          if (active) setServiceOnline(false);
        });
    };

    checkHealth();
    const interval = window.setInterval(checkHealth, 15_000);
    return () => {
      active = false;
      window.clearInterval(interval);
    };
  }, []);

  return (
    <div className="app-shell">
      <aside className="sidebar" aria-label="主导航">
        <a className="brand" href="#files" onClick={(event) => { event.preventDefault(); navigateTo('files'); }}>
          <span className="brand-mark"><Folder size={20} strokeWidth={1.8} /></span>
          <span><strong>FileSage</strong><small>个人文件工作台</small></span>
        </a>
        <p className="nav-label">工作区</p>
        <nav className="side-nav">
          {navigation.map(({ id, label, Icon }) => (
            <button key={id} className={page === id ? 'nav-item active' : 'nav-item'} aria-current={page === id ? 'page' : undefined} onClick={() => navigateTo(id)}>
              <Icon size={18} strokeWidth={1.8} /><span>{label}</span>
            </button>
          ))}
        </nav>
        <section className="volume-card" aria-label="存储状态">
          <div className="volume-title"><HardDrive size={15} /><span>文件服务</span><span className={`status-dot${serviceOnline ? '' : ' is-offline'}`} /></div>
          <strong>{serviceOnline ? '服务在线' : '服务未连接'}</strong>
          <small>{serviceOnline ? '当前连接到本机管理根目录' : '后端启动后读取服务器目录'}</small>
        </section>
        <button className="settings-link" type="button" aria-current={page === 'settings' ? 'page' : undefined} onClick={() => navigateTo('settings')}><Settings2 size={17} />Agent 设置</button>
      </aside>

      <main className="main-panel">
        <header className="topbar">
          <div className="mobile-brand"><span className="brand-mark"><Folder size={17} /></span><strong>FileSage</strong></div>
          <span className="service-indicator"><i className={`status-dot${serviceOnline ? '' : ' is-offline'}`} />{serviceOnline ? '文件服务在线' : '服务未连接'}</span>
        </header>
        <div className="page-content">
          <Suspense fallback={<div role="status">正在加载页面…</div>}>
            {page === 'files' && <FilesPage onOpenTrash={() => navigateTo('trash')} onOpenAgent={(context) => { setAgentLaunchContext(context); navigateTo('agent'); }} />}
            {page === 'photos' && <PhotosPage />}
            {page === 'trash' && <TrashPage />}
            {page === 'agent' && <AgentPage launchContext={agentLaunchContext} onLaunchContextConsumed={() => setAgentLaunchContext(null)} />}
            {page === 'tasks' && <TasksPage />}
            {page === 'settings' && <AgentSettingsPage />}
          </Suspense>
        </div>
        <nav className="mobile-nav" aria-label="手机主导航" style={{ gridTemplateColumns: 'repeat(5, minmax(0, 1fr))' }}>
          {[...navigation, { id: 'settings' as const, label: '设置', Icon: Settings2 }].map(({ id, label, Icon }) => (
            <button key={id} className={page === id ? 'mobile-nav-item active' : 'mobile-nav-item'} aria-current={page === id ? 'page' : undefined} onClick={() => navigateTo(id)}>
              <Icon size={19} strokeWidth={page === id ? 2.2 : 1.8} /><span>{label}</span>
            </button>
          ))}
        </nav>
      </main>
    </div>
  );
}
