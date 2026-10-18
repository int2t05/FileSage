# FileSage 技术决策基线与 ADR

初始决策日期：2026-10-15。状态更新：2026-10-18。**技术方向已选，主要实现与 Linux 验收已开展；目标服务部署、真实样本和容量基线仍未全部完成。**用户授权助手决定 ADR-0001～0006、0008 的技术取舍；ADR-0007 为用户指定 React 后按授权定稿。以下状态区分自动化／隔离服务证据与目标主机验收，不代表整体成熟。对应[成熟需求基线](PRD.md)。

## 已选择的技术基线

采用单机模块化单体：Go 1.27.2 或更新的安全补丁版后端 + React／TypeScript／Vite 前端；SQLite 配 WAL，由纯 Go `modernc.org/sqlite` 驱动保存可重建索引及任务／审批状态；原生挂载文件系统仍是文件内容唯一事实来源。图片、文本、PDF、DOCX、EPUB、表格使用格式专用阅读组件；PPTX 由受限 LibreOffice 生成只读派生预览；OCR、解码和归档工具按任务启动为本机子进程。

对 Agent 写入，先生成经批准的明确操作，再由统一文件服务协调、检查状态并提交；普通文件内容采用同目录临时文件加替换，跨盘移动采用复制、校验、发布、再删除源文件的持久任务。写协调同时使用路径和 `(device, inode)`，覆盖进程内硬链接别名竞争；不阻止其他进程直接修改文件，关键操作在提交前复核身份与内容状态。通过内部软链接读取和列目录，写入须针对明确的目标路径。Shell 只在 Bubblewrap 隔离的暂存副本中运行，审批展示实际差异，批准后才由统一提交器写回。原件旧内容不进入历史或备份。tusd 只接收可续传上传到私有暂存区；最终目录发布由 FileSage 协调器完成。

无密码访问是已确认需求；可达服务的设备拥有完整能力。服务默认仅监听回环地址，部署到局域网时需显式绑定指定私有网卡地址并配置网络防火墙。服务以专用非 root 用户运行，实际读写范围由挂载点与 Unix 权限共同限定。硬链接内容写入、SQLite 驱动、上传协议、沙箱不可用时的策略及首轮性能门槛均已由受托技术负责人选定，具体证据和实施验证见下文与各 ADR。

## 部署形态与边界

```mermaid
flowchart LR
  B[局域网浏览器<br/>桌面／手机] -->|同源 HTTP API| A[Go 模块化单体]
  A --> F[文件服务<br/>范围校验与协调提交]
  F --> R[管理根目录<br/>真实挂载文件系统]
  A --> D[(SQLite<br/>索引、任务、审批、审计元数据)]
  A --> Q[有界后台任务池]
  Q --> H[LibreOffice／Tesseract／ExifTool／归档与媒体工具]
  A --> M[本地模型或用户配置的远程模型]
  Q --> C[预览与 OCR 派生缓存]
```

| 边界 | 建议约束 | 失败表现 |
| --- | --- | --- |
| 用户文件 | 仅通过统一文件服务读写；管理根目录和其内已挂载卷是操作范围；根目录之外不作为存储目标 | 越界、悬空链接、循环、卸载卷或权限错误均明确失败，不回退到空目录 |
| 服务状态 | SQLite、配置、日志、临时任务状态放在独立服务数据目录，不放入用户整理范围；预览/OCR 是可删可重建的派生缓存 | 状态库损坏时停止写操作并提供诊断；重建索引不能覆盖原生文件 |
| 本机工具 | 以无特权服务用户运行；按任务调用独立进程，清理环境变量、限定输入输出路径、资源和时限 | 工具缺失或不支持时只禁用相应预览／任务，不影响基础文件管理 |
| 模型服务 | 仅通过显式配置的本地或远程端点；调用前说明会发送的内容范围；密钥仅在服务端保存 | 超时、限流或不可用时保留分析／任务错误，不阻塞浏览、上传和手动操作 |
| 网络 | 按用户需求提供局域网访问，无账号密码；开发／默认配置仅监听 `127.0.0.1`，部署时显式设置 `FILESAGE_LISTEN_ADDR` 为服务器私有网卡地址，并通过 Host 白名单接受实际访问域名 | 客户端断线不改变已提交结果；所有长任务可查询状态 |

无认证意味着任何能够访问服务的设备都可以执行删除、权限调整、Shell 等高影响操作。服务入口按规范化 Host 主机名／IP 白名单拒绝未知 Host，以降低 DNS rebinding 风险；写请求继续检查 Origin。白名单默认包含监听 IP 与 `localhost`、`127.0.0.1`、`::1`，`FILESAGE_ALLOWED_HOSTS` 可追加逗号分隔的主机名或 IP，端口不参与匹配。Origin／Fetch Metadata 检查不能限制同一局域网内的其他设备；局域网部署仍须绑定指定私有网卡并配置防火墙。服务目前只提供 HTTP 直连，不读取 `X-Forwarded-Proto`；HTTPS 终止型代理向后端转发 HTTP 时，同源写请求会被拒绝，因此该部署方式暂不支持。产品遵循 PRD，不额外加入密码。

## 核心选型对比

### 服务端

| 方案 | 适配点 | 主要代价 | 结论 |
| --- | --- | --- | --- |
| Go 模块化单体 | 有标准库 HTTP、并发、单一服务部署；Go 1.24 起有根目录约束 API；已有 tusd、文件浏览竞品与 Go SQLite 驱动可参考 | 文档解析仍需专用库或本机工具；`os.Root` 有版本相关安全修复，不能固定在含漏洞版本 | **建议**，依赖 Go 1.27.2 或更新且保持安全补丁；上线构建前查官方支持与漏洞公告 |
| Python + FastAPI | 文档、图像、OCR、Office 处理生态广；Paperless、Docling、Unstructured 等有可读实现参考 | 原生文件边界与 TOCTOU 安全需更细致实现／额外库；运行时、依赖和打包面更大 | 适合以解析和 AI 数据处理为主的团队；本产品的高权限文件写路径更难收口 |
| Go API + Python 工作服务 | 文件服务保持 Go；可直接复用 Python 解析库 | 两套运行时、进程协议、部署和资源治理；单用户 MVP 的故障面扩大 | 仅当原型证明 Go 进程工具调用无法覆盖 DOCX/OCR 需求时再拆 |
| Rust + Axum | 编译期内存安全，异步 HTTP 生态可用 | 本地文件／文档集成仍需跨语言工具；实现和调试门槛未知，未观察到本项目团队经验 | 可行备选；没有证据显示它比 Go 在本需求上有足够收益 |

建议先保持**单个 Go 服务进程**，把文件、上传、索引、Agent 审批和任务生命周期作为内部模块，不拆微服务。LibreOffice、Tesseract、ExifTool、归档／媒体工具是按任务启动、受限且可失败的子进程，不构成常驻服务集群。Go 官方 `os.Root` 能约束路径与越界软链接，但 2026 年曾公布涉及末尾斜杠软链接的越界漏洞；使用前须确认选用版本已包含修复，并为软链接路径写回归验证。[Go `os.Root` 设计说明](https://go.dev/blog/osroot)、[官方漏洞公告 GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970)、[Go 发布与支持记录](https://go.dev/doc/devel/release)。

### 前端

前端定为 React + TypeScript + Vite，并由 Go 服务同源提供静态资源。基础对话框、菜单和选择控件使用 Radix Primitives；业务组件和样式由 FileSage 自行维护，不引入完整后台模板。图标统一使用 `lucide-react`。界面布局、手机端四入口和关键流程见已定稿的 [UI/UX 基线](design/UIUX.md)；[交互原型](design/prototype/README.md)用于审阅布局与示例状态，不代表生产功能或真实手机验证已经完成。

### 元数据、索引与后台任务

| 方案 | 优点 | 代价 | 结论 |
| --- | --- | --- | --- |
| SQLite（本机服务数据盘） | 无独立数据库服务；单实例部署简单；WAL 支持读写并行、事务保持一致快照；FTS5 可提供路径子串索引 | 同一时刻仍只有一个写事务；WAL 不适用于网络文件系统；数据库不可与 WAL/SHM 文件拆开复制 | **建议**存索引、任务、审批、审计元数据；数据库放本机文件系统而非用户数据挂载卷 |
| PostgreSQL | 多进程和并发写能力强，成熟查询能力 | 另装、配置、升级和备份服务；对单用户单机暂时没有已测出的必要负载 | 若后续多进程部署或 SQLite 写入实测成为瓶颈，再迁移 |
| 不建持久索引／每次遍历 | 状态始终即时读取真实文件系统；组件最少 | 大目录搜索、媒体筛选和外部改动协调成本高；无法高效恢复长任务状态 | 不适合 PRD 的搜索、并发和任务恢复 |

文件系统仍是权威数据。SQLite 的目录记录可全量扫描重建；任务、审批及审计记录必须事务保存，但只含操作元数据和结果，不保存旧文件全文／差异。选用 `modernc.org/sqlite` 纯 Go 驱动，以免服务构建依赖 CGo；驱动内置的 SQLite 核心也纳入版本审查和 SBOM。WAL 允许多读者和单写者并行，并要求同机共享内存；文件变化监听用 Linux inotify 及时更新，遇队列溢出、卸载或重启后执行一致性扫描。名称和路径查询使用 SQLite 索引／FTS5；真实规模基准决定短词回退策略。拍摄日期目前由 `github.com/bep/imagemeta` 0.17.2 从支持图片的 EXIF／XMP 元数据读取，并在 JPEG、TIFF 和 RAW 格式尝试 IPTC；迁移前完成的索引代次会标记为不含拍摄元数据，须等待一次完整新扫描后才可筛选。[SQLite WAL 并发与网络文件系统限制](https://www.sqlite.org/wal.html)、[SQLite FTS5 trigram](https://www.sqlite.org/fts5.html)、[inotify 丢事件与重建缓存](https://man7.org/linux/man-pages/man7/inotify.7.html)、[modernc.org/sqlite 驱动和嵌入版本信息](https://pkg.go.dev/modernc.org/sqlite)、[imagemeta 上游](https://github.com/bep/imagemeta)。

后台任务先用同进程有界 worker pool + SQLite 持久任务表。任务需有状态机、租约／心跳、取消请求、每项结果和幂等提交标识；重启后恢复未完成任务时先核对文件状态，绝不把“再次运行整个批次”当作恢复。暂不加 Redis 队列；Asynq 展示了 Redis 队列的成熟任务模式，但也引入独立 Redis 运维。若一台主机上出现多个 FileSage 服务进程，再重新评估。[Asynq 本地采集](https://github.com/hibiken/asynq/blob/d135f1439bee74e989b7f9b41ecd542cc87f024a/README.md)、[Go SQLite 驱动对照：cgo 与纯 Go](https://github.com/mattn/go-sqlite3/blob/dbbe931904f2ee06546448fbe7cf7780f61bc110/README.md)、[modernc SQLite 及其依赖／许可说明](https://github.com/modernc-org/sqlite/blob/0fc3f90d9e280a7356d8609520bc2ba081bfc248/README.md)。

### 原生文件操作、写协调与恢复

所有应用写入口都调用同一文件操作层，输入为根目录相对路径和预期文件状态，不接收可直接拼接的任意绝对路径。使用基于目录句柄的安全路径解析；优先评估 Go `os.Root`，Linux 专属实现可用 `openat2` 的 `RESOLVE_IN_ROOT` 限制软链接解析边界。管理根目录内的挂载卷允许跨挂载点，因此不能以禁止所有 mount crossing 的标志作为边界措施。[Linux `openat2`](https://man7.org/linux/man-pages/man2/openat2.2.html)、[Go traversal-resistant file API](https://go.dev/blog/osroot)。
- 同一对象使用路径队列加 `(device, inode)` 身份协调；父目录操作与其子项冲突，同路径等待后重新核对。外部工具无法被应用锁住，执行前比对类型、大小、纳秒时间、inode 等状态；关键替换可再核内容哈希。
  - 所选文件／目录 ZIP 下载在 Linux 上使用 `openat` 与 `O_NOFOLLOW` 逐项遍历，不跟随软链接，拒绝特殊文件和跨平台歧义名称；最多遍历 128 层，打包期间复核文件与目录状态，并先写入权限受限的临时文件，完整关闭和同步后才发送。当前限制为每次 256 项、5 万个归档条目、20 GiB 源内容和一个并发任务；超限整包失败，Windows 明确返回不支持。归档创建复用该 no-follow 遍历与限额，支持 ZIP 存储、TAR、gzip TAR.GZ 和 7z。7z 使用安全 TAR 流暂存到私有临时目录，再调用服务器 7-Zip 创建，临时空间需容纳暂存内容与归档；所有格式完整生成后复核来源并原子无覆盖发布，与 ZIP 下载共用一个并发槽位。
- 新内容先写入同一目标文件系统的暂存文件，校验完成后以原子重命名发布，读者不会看到半成品。只有新内容和操作元数据进入暂存／审计；旧内容不备份。替换发布后不能恢复旧内容。
- 同文件系统目录改名可以原子提交；跨盘移动不是单次原子事务。记录复制、校验、发布、源删除各阶段；出错时保留源、标记部分完成，并允许按状态继续或人工处置。
- 每个已批准操作绑定方案 ID、目标路径、源状态及目标冲突策略。状态变化、方案变化或目标被占用时使审批失效；重新审批后才能继续。
- 软链接只允许跟随到根目录内，超界、悬空和循环均明确报错；移动／删除链接本身与操作目标分开呈现。打开链接目标用于内容编辑时，使用经验证的目标路径及其 `(device, inode)` 锁；若目标无法安全规范化则拒绝写入。多硬链接文件（`nlink > 1`）允许读取、按路径重命名和移除链接项，但所有手动／Agent 内容编辑、覆盖和 Shell 写回均拒绝，避免破坏别名一致性。

## 上传、阅读和 Agent 执行

### 上传

浏览器使用 `tus-js-client`，服务端内嵌 tusd v2 的 tus 1.0 handler，上传仅写入服务私有暂存区。服务端分别持久化接收与发布状态；完成后验证长度、哈希、目标状态和权限，再经协调器改名发布；跨盘时复制并校验。回调不得绕过数字后缀、目标冲突和文件写锁。浏览器重选文件后重新核对大小与内容指纹，不能按文件名复用续传状态。[tus 协议](https://tus.io/protocols/resumable-upload.html)、[tusd v2 上游](https://github.com/tus/tusd)、[浏览器客户端](https://github.com/tus/tus-js-client/blob/0fee69e35750090a6f7f9ea30abf08852f0253ca/README.md)。分片大小、校验方式、同时上传数和文件规模按性能基准决定。

### 文件阅读与派生处理

采用按格式组合，不引入完整 Office 协作套件。PDF 使用 PDF.js；文本／Markdown 使用 CodeMirror 6、Markdown 渲染器加消毒；DOCX 在浏览器 Worker 中受限解析正文、标题、表格和 PNG／JPEG 图片；EPUB 使用专用解析器；XLSX 显示已缓存值并按需读取工作表；PPTX 通过隔离的 LibreOffice 生成只读 PDF。DOCX 仅按文档显式分页符导航，不模拟 Word 动态排版；宏、外链和文档脚本不执行，归档内 DOCX 可用同一阅读器。[PDF.js 阅读 API](https://mozilla.github.io/pdf.js/getting_started/?lang=en)、[LibreOffice 转换过滤器](https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html)、[SheetJS 官方 HTML／表格资料](https://docs.sheetjs.com/docs/api/utilities/html/)、[现有 Office 与 EPUB 覆盖表](../reference/technical-coverage.md)。

Office 两条路径都需真实样本验证：浏览器解析可保持工作表／章节等结构但图文版式有限；LibreOffice 更接近分页阅读但会增加运行时、字体差异和转换隔离成本。ONLYOFFICE DocumentServer 覆盖面广，但引入完整独立服务和 AGPLv3 许可审查，并超出前端只读所需范围，暂不推荐。[DocumentServer 上游许可证](https://github.com/ONLYOFFICE/DocumentServer/blob/master/LICENSE)。

### Agent 与 Shell

Agent 以工具 API 产生带稳定 ID 的类型化操作清单，不接触文件服务账户、任意路径或数据库。模型返回的路径、命令、文本和解析内容全部视为不可信数据；服务端独立校验根目录、权限、冲突、方案状态和审批记录。文档抽取/OCR 限页数、字节、像素和运行时间，并显示未读范围和失败条目。当前 Agent 分类支持文本、PDF、DOCX 与 PPTX：PDF 使用 Bubblewrap 隔离的 Poppler，DOCX／PPTX 复用隔离 LibreOffice 转 PDF；每份最多前 20 页、PDF／Office 单件 16 MiB、单件正文 64 KiB、整批正文 256 KiB。扫描件 OCR 默认关闭；用户在分类确认卡主动选择后，每批最多处理 2 份 PDF／Office，仅对整份无文字层文档识别前 20 页，使用系统 Tesseract（chi_sim+eng）与 Poppler。Agent 分类分析最多并发 2 批，并在读取正文前占用处理槽。每页先渲染、识别和清理，单页图像限 4 MiB、累计渲染限 64 MiB、总时限 60 秒，并在 1 GiB 地址空间、Bubblewrap 无网络沙箱内运行；混合文字与扫描页暂不逐页补识别。建议本地模型先兼容 Ollama 常用 API，远程端点由用户配置；模型适配器需把提供商的工具调用转换为相同 JSON 操作格式，不让特定模型 SDK 成为文件权限层。[Ollama 本地项目](https://github.com/ollama/ollama/blob/8e4dad49f9f527762610fd419ee2ae39207f67a4/README.md)、[llama.cpp 服务参考](https://github.com/ggml-org/llama.cpp/blob/3e390af18de1131dda27913c00511725b4274c8a/README.md)、[文档抽取／OCR 参考](../reference/technical-coverage.md)。

Shell 属于高风险操作，当前实现采用两阶段审批：用户先确认命令后，Agent 将选定范围复制到服务端私有基线和工作区，再由 Go 服务启动 Bubblewrap／prlimit 子进程，在 Linux 用户、PID、网络等隔离命名空间内执行。沙箱只读挂载基线、可写挂载工作区，不继承宿主环境或网络；命令进程加入 systemd 委托的 cgroup v2，并受 CPU、内存、进程、时间、输出和暂存空间限制。执行后逐项比较差异并形成新增／修改／删除清单；逐项审批和再次确认后，由统一写回协调器应用，并复核原件状态。无法建立隔离或资源限制时拒绝执行；差异无法核验或原件发生变化时拒绝写回。目标主机必须验证 Bubblewrap 与 systemd 委托能力；条件不足时拒绝运行，不退化为直接执行。Bubblewrap 不保证任意命令本身安全，也不承担 FileSage 的审批逻辑。[Bubblewrap 参考](https://github.com/containers/bubblewrap/blob/1fb0aa26d3eb66254f7742e704add7f82f41a582/README.md)、[Linux Landlock 参考](https://man7.org/linux/man-pages/man7/landlock.7.html)。

Shell 暂存方案让用户审查真实文件差异。拒绝 Shell 的替代方案更安全但不满足已确认范围；直接修改原目录无法可靠列明影响或统一协调，因此不采用。

当前实现边界：`internal/shellstage` 提供 Linux Bubblewrap 暂存执行、差异清单和 cgroup v2 聚合限制；`internal/agent` 已接入对话确认、隔离运行、差异预览、逐项审批及统一写回，HTTP 路由和 React 审批界面也已接通。用户确认命令后才运行隔离副本，写回前会复核原件状态；命令不会自动重跑。systemd 模板委托 `cpu`、`memory`、`pids`；服务把自身移入叶组，并通过 clone3 将命令进程原子放入独立任务组。每项任务设置 `memory.max`（禁用交换区、OOM 整组终止）、`pids.max` 和单核 `cpu.max`；另每 100 ms 检查聚合 CPU 用量，超过预算即终止。cgroup v2 没有累计 CPU 秒控制器，因此 CPU 累计预算依赖运行时检查，主机严重调度延迟时可能超出检查间隔；并保留 `prlimit` 单进程 CPU／地址空间／进程／文件大小限制、墙钟超时、暂存／临时空间和输出限制。缺少委托、控制器、clone3 入组或清理能力时拒绝执行。Linux 服务器的临时委托 systemd 测试单元通过 Shellstage 全量集成测试；未委托的 SSH 会话实测拒绝执行。Agent Shell 的 Linux 自动化闭环用例见 `internal/agent/shell_linux_test.go`；目标主机实际安装 FileSage systemd unit 和浏览器端完整流程仍待验收。

## 已选性能与可靠性基线

下列为**已选技术验收门槛，尚未测量**。用一台普通 Linux 服务器、SSD、本机多挂载点、千兆有线 LAN 和可复用真实样本，记录硬件、文件系统、Go／SQLite 版本、浏览器、文件数量与大小分布。首轮按 10 万目录项、单目录 1 万项、20 GB 大文件和 20 个并行浏览／任务请求做容量基线；这些是验证样本与目标，不设置产品硬上限。

| 指标 | 建议门槛 | 测量边界 |
| --- | --- | --- |
| 已索引目录首屏 | 1 万项目录分页首屏 p95 ≤ 500 ms | 不含首次全盘扫描；分别测 SSD 与单独慢盘 |
| 文件名／路径筛选 | 10 万条索引 p95 ≤ 1 s；1–2 字符短词单独测量 | 无全文内容／向量索引，不得把首次建索引混入响应时间 |
| 上传 | 稳态吞吐达到同客户端到同一挂载卷直接写入基线的 ≥80% | 同时报告单大文件、小文件批次、哈希校验、CPU 和峰值内存；不跨盘比较 |
| 续传／发布 | 断网、浏览器刷新和进程重启后恢复已收字节；完成目标无截断／静默覆盖 | 每次对比源文件和发布文件 SHA-256；混合 mount 情形单独记录 |
| 阅读与后台任务 | 首屏浏览及上传不因单个 OCR／转换任务被阻塞 | 记录队列等待、首屏时间、CPU、内存；派生任务有资源上限 |
| 故障恢复 | 进程在每个提交阶段被终止，重启后不重复已完成项、不覆盖变化后的文件 | 真实多挂载点、满盘、只读盘、拔除／重新挂载和权限不足场景 |

以上目标由助手受托选为初始验收阈值，尚未测量，不代表产品已经达标。若实现证据显示阈值不适合，再记录实测结果和调整理由。

## 决策状态与验证门槛

| ID | 检查问题 | 推荐／现状 | 关闭标准 | 实现／部署状态 |
| --- | --- | --- | --- | --- |
| ADR-0001 | 服务端语言、进程边界与安全版本 | Go 1.27.2+，模块化单体；格式工具受限子进程 | Linux 构建通过；官方安全版本巡检、越界回归测试通过 | Linux 构建与路径回归通过；安全公告复核和目标部署待验 |
| ADR-0002 | 辅助状态持久化 | 本机数据目录 SQLite WAL；`modernc.org/sqlite` 纯 Go 驱动 | 服务数据不落入管理根目录；迁移、重启恢复及真实规模基准通过 | 迁移与恢复自动测试通过；目标数据目录核验、容量基准待验 |
| ADR-0003 | 软／硬链接写入语义 | 内部软链接安全跟随和目标协调；`nlink > 1` 的内容编辑、覆盖、Shell 写回拒绝；链接项路径操作独立 | 越界链接、替换竞态、多硬链接样例通过真实 Linux 测试 | 链接回归与双物理卷隔离服务验收通过（旧构建）；当前部署、非 root 权限、外部进程竞争待验 |
| ADR-0004 | 大文件续传和发布 | tus 1.0 + tusd handler，接收至私有暂存后由 FileSage 校验发布 | 刷新／断网／重启续传；单盘与跨盘哈希一致且不静默覆盖 | 重启恢复与 SSH 隧道中断 E2E 通过；浏览器选文件断网续传、压力基线待验 |
| ADR-0005 | 多格式只读阅读器 | 专用浏览器解析 + 受限 LibreOffice DOCX/PPTX 派生预览；Office 样本矩阵作为启用门槛 | 覆盖真实中文图文样本；保留失败原因；不得执行宏／外链 | Office／归档 Linux HTTP 验收通过；RAW/HEIF 服务端点及复杂真实样本待验 |
| ADR-0006 | Agent Shell 审批隔离 | Bubblewrap 只读根目录 + 私有写暂存 + 差异审批 + 统一提交；能力不足时 fail-closed | Linux 服务器临时委托 systemd 测试单元通过 cgroup 聚合 CPU、内存、PID 与原子入组集成测试；代码已接入 Agent/API 审批及统一写回，目标主机实际服务部署和浏览器端完整流程仍待验收 | 委托 systemd 沙箱原语通过；FileSage 服务部署与浏览器闭环待验 |
| ADR-0007 | 前端框架与交互原语 | React + TypeScript + Vite；Radix Primitives、Lucide | 生产构建通过；关键页面窄屏与键盘交互可用 | 构建和 320/360/390px 视口检查通过；实体手机与完整无障碍验收待完成 |
| ADR-0008 | Agent 文本编辑候选暂存 | 正文私有暂存；数据库与审计仅留摘要；审批和执行分离 | Linux E2E 覆盖隐私、版本冲突、拒绝、恢复和候选清理 | 实现与 Linux E2E 通过；边界见对应 ADR 和验收台账 |
| AUD-01 | 无密码服务的局域网暴露边界 | 默认回环监听；部署时显式绑定私有网卡；状态写请求校验 Origin；不提供公网绑定指引 | 部署文档、启动日志和浏览器跨站请求验证一致 | Host/Origin 自动测试通过；当前构建的目标主机绑定与防火墙待验 |
| AUD-02 | 服务用户文件系统权限 | 专用非 root 用户；只授予管理卷所需读写权限；不提权；`chown` 遵循内核权限 | 非 root 多挂载点测试与权限不足错误信息通过 | 双物理卷跨卷操作已验证；专用非 root 账户与挂载权限待验 |
| AUD-03 | 首轮容量和性能门槛 | 10 万项索引、1 万项单目录、20 GB 上传、20 并发；初始目标，暂无测量 | Linux SSD 与多挂载点基准记录设备、样本及分布 | 尚无该规模的基准数据 |

实现阶段的验证门槛：

| ID | 问题 | 影响 | 建议 | 决策者／阶段 |
| --- | --- | --- | --- | --- |
| OPN-01 | `nlink > 1` 文件的写操作是否被拒绝？ | 硬链接内容与目录项不可同时原子替换 | 真实 Linux 硬链接样例确认 Agent、手动编辑、覆盖和 Shell 均拒绝，移动／删除路径结果明确 | 实现验证／发布前 |
| OPN-02 | 目标 Linux 主机是否具备 Bubblewrap 与委托 cgroup v2 能力？ | Agent Shell 的部署前置条件 | 缺少 Bubblewrap、控制器、clone3 入组或清理能力时拒绝运行，不得降级为直接执行 | 实际 FileSage systemd 部署验收 |

ADR-0001～0006、0008 和 AUD-01～03 的方向由助手依据用户授权选定；ADR-0007 与 UI/UX 基线依用户明确选择采用 React 并由助手完成设计。这里记录选择理由和可验证的关闭标准，不把文档审阅写成实现通过。若后续审计发现需要调整，再按编号记录新决定及迁移影响。

## 资料与适用性

本地调研批次核验了 110 个 GitHub 项目、1,308 份固定提交文档及 SVG、26 个官方网页快照。公开仓库发布研究结论、来源链接、哈希清单和采集工具，不分发第三方原文；清单与核验结果见[调研索引](../reference/README.md)、[仓库索引](../reference/catalog.md)、[技术覆盖矩阵](../reference/technical-coverage.md)和[核验报告](../reference/verification.md)。与推荐直接相关的实现参考包括：[Quantum 文件管理器](https://github.com/gtsteffaniak/filebrowser/blob/91bc608f0ec6a3246c29e5fc87acdd46a5fadf44/README.md)、[Copyparty 传输／文件服务](https://github.com/9001/copyparty/blob/0dc1cb329120743ca85599ba3fba24849f137677/README.md)、[tusd](https://github.com/tus/tusd/blob/78cc2291823e171b20d915570e3b68f554eab908/README.md)、[Google safeopen](https://github.com/google/safeopen/blob/43626d6f468560fc611ad54f1f17f3a455dbe92d/README.md)、[Go SQLite 驱动](https://github.com/mattn/go-sqlite3/blob/dbbe931904f2ee06546448fbe7cf7780f61bc110/README.md)、[FastAPI](https://github.com/fastapi/fastapi/blob/49a09a0d4e876b4e7d6d87fc049710263aaeaa7e/README.md)、[Axum](https://github.com/tokio-rs/axum/blob/e57b547eb95de74b73b34719dd5926254a085634/README.md)、[Vue](https://github.com/vuejs/core/blob/bf5f0dd877dd3c23ce85c2cbc1dd771567e84c8c/README.md)、[React](https://github.com/facebook/react/blob/99c106c43439ad7af05d42f66582d8d05190e9ff/README.md)、[浏览器上传客户端](https://github.com/tus/tus-js-client/blob/0fee69e35750090a6f7f9ea30abf08852f0253ca/README.md)、[本机文件监听](https://github.com/fsnotify/fsnotify/blob/20b1e15ef3c70caeb37ea2bd184f48ef8382669e/README.md)、[本地模型服务](https://github.com/ollama/ollama/blob/8e4dad49f9f527762610fd419ee2ae39207f67a4/README.md)、[图像处理](https://github.com/libvips/libvips/blob/06564b0a4c3f473d0e0cb9f9977ab647888aa43d/README.md)及[归档工具](https://github.com/mholt/archives/blob/9bf66b8e091c2318c7de7b2054c19ddf7b2702ce/README.md)。

资料采集固定至上游提交并保存哈希，不能证明实现的能力、维护质量或兼容性。已选组件仍须在许可证、维护状态、Linux 目标版本、恶意输入、安装体积和真实样本保真度上做实现级核验；阅读器中未锁定的解析依赖在核验前不得进入发布构建。纯 Go SQLite 驱动需要把其生成的 SQLite 核心版本纳入漏洞巡检与 SBOM。不得把源码公开当作可任意复用。
