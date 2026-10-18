# FileSage 竞品调研与需求提炼

竞品基线日期：2026-10-14；技术选型补充日期：2026-10-15。已确认需求见 [PRD](../docs/PRD.md)，候选架构见[待审计技术方案](../docs/TECH.md)。当前仓库总数及固定提交、文件来源见 [项目索引](catalog.md) 和 [采集清单](manifest.json)。内容编辑、软链接、Shell、分类阅读能力及并发行为的技术参考见 [技术覆盖说明](technical-coverage.md)。

## 结论

FileSage 的定位是自用、局域网内访问的原生文件系统管理器，叠加照片与文档预览，以及人工审批的 AI 整理。最贴近的组合参照是 FileBrowser Quantum、NextExplorer、Copyparty 的文件能力，PiGallery2 的目录浏览，以及 AI 整理工具的操作建议与撤销。

照片平台、文档平台和个人云覆盖了许多成熟场景，但其账号、资料库、标签、同步或专用存储模型，不能直接替代用户要求的真实目录。全量整合这些平台会引入多套数据模型和服务生命周期；本项目应提炼行为，并保持一套文件操作和任务执行机制。这是针对 FileSage 的产品判断，不是对竞品质量的排序。

## 证据范围

竞品基线采集 87 个仓库；技术选型时补充 15 个后端、前端、数据库、上传、任务、监听、模型和媒体工具仓库。所有仓库均取得 README；重点项目读取固定提交的目录树和相关文档。另保存官方网页及文档查询快照。当前具体数量、文件完整性与缺失项以清单和验证报告为准。

资料属于文档快照，没有部署或运行这些竞品。以下“支持”表示上游文档描述，不表示实测通过。没有证据时标记为未据此确认，不以缺失推断功能不存在。自动关键词命中用于检索，不能作为功能确认；规划文档、未完成事项和支持矩阵的实际状态需一起阅读。

未抓取完整源码、依赖、模型权重、演示媒体或所有历史 Issue。未按 Star 数量排名；公共 API 限流，因此没有编造热度、更新时间或统一维护状态。每个上游文件保留来源 URL、固定提交和 SHA-256。README 内的外链与图片可能需要联网，局部采集的文档也可能引用未下载页面。

## 重点项目分析

| 项目与证据 | 上游文档描述 | FileSage 应提炼的需求 | 不直接引入的范围或适配限制 |
| --- | --- | --- | --- |
| [File Browser](https://github.com/filebrowser/filebrowser/blob/833d908884d5c801f30f5c098d7977177eb3a36b/README.md) | 在指定目录上传、删除、预览、编辑；README 明示已归档 | 指定根目录即可管理已有文件，基础入口保持简单 | 归档项目作为交互参考，不预设为实施底座 |
| [Quantum](https://github.com/gtsteffaniak/filebrowser/blob/91bc608f0ec6a3246c29e5fc87acdd46a5fadf44/README.md)、[来源配置](https://github.com/quantumx-apps/filebrowserDocs/blob/cdd37c97b104b817d1f5b09ac0069ce3292d878d/content/en/docs/configuration/sources.md) | 多来源、搜索、响应式界面、预览和配置 | 多磁盘可见、搜索筛选、文件类型预览、可配置部署 | 多账号、复杂认证、分享权限不符合本期自用范围 |
| [Quantum 索引](https://github.com/quantumx-apps/filebrowserDocs/blob/cdd37c97b104b817d1f5b09ac0069ce3292d878d/content/en/docs/features/indexing.md) | 索引刷新、扫描成本、默认不跟随软链接 | 文件系统为准，索引异步；链接的范围、循环和别名须明确处理 | FileSage 需支持根目录内软链接，不能直接采用此默认行为；数据库调优须实测 |
| [NextExplorer](https://github.com/nxzai/NextExplorer/blob/662fb42986689f0cc14b634e603b56ff674e3471/README.md)、[回收站](https://github.com/nxzai/NextExplorer/blob/662fb42986689f0cc14b634e603b56ff674e3471/docs/admin/trash.md) | 多挂载入口、列表与网格、媒体预览、搜索；各卷同盘回收站 | 多磁盘浏览、批量操作、本盘删除与恢复、崩溃后核对任务状态 | OIDC、访客、外链分享、多人协作不进入 MVP |
| [goDrive](https://github.com/MrCodeEU/goDrive/blob/54d7af59be21b02d73868fab4fc3cbdb8f4caeb9/README.md)、[功能状态](https://github.com/MrCodeEU/goDrive/blob/54d7af59be21b02d73868fab4fc3cbdb8f4caeb9/docs/feature-support.md) | 文件系统为准、可恢复上传、预览缓存、外部变更扫描；项目仍在完善 | 可恢复上传、原子发布、可重建索引与预览、任务状态 | README 的能力描述须结合功能状态；不把未实测产品当成生产成熟性证明 |
| [Copyparty](https://github.com/9001/copyparty/blob/0dc1cb329120743ca85599ba3fba24849f137677/README.md) | 可恢复上传、文件夹上传、批量重命名、媒体与 Markdown、ZIP 下载、检索 | 可靠大文件上传、可见传输队列、下载与批量管理 | 多协议服务、服务器钩子、复杂选项不属于本项目目标；不采用软链接去重 |
| [Dufs](https://github.com/sigoden/dufs/blob/3a793a0fa60a043b80f35651d7405364fb1b5c1c/README.md) | 文件与文件夹上传、续传、下载、搜索、编辑 | 单服务完成常用操作，减少部署依赖 | WebDAV、账号授权仅作相邻能力参考 |
| [Filestash](https://github.com/mickael-kerjean/filestash/blob/64b1e4ba0e53c16bb41671c826ff1127e10b82d7/README.md) | 多存储接入、多格式查看、搜索和扩展 | 文件详情统一入口、不支持格式明确降级 | 存储适配器、插件市场和网关不进入 MVP |
| [PiGallery2](https://github.com/bpatrik/pigallery2/blob/dc456950250bfddf7efae9270fe64658606911c9/README.md)、[功能](https://github.com/bpatrik/pigallery2/blob/dc456950250bfddf7efae9270fe64658606911c9/docs/features.md) | 目录优先、低资源照片浏览；照片目录只读 | 照片视图仍按真实文件夹组织，缩略图异步生成 | 其只读行为不能满足 FileSage 的移动、重命名和整理 |
| [Immich](https://github.com/immich-app/immich/blob/9ba71d908d2d90d9b9d51241e8c2091b7263be6e/README.md)、[外部资料库](https://github.com/immich-app/immich/blob/9ba71d908d2d90d9b9d51241e8c2091b7263be6e/docs/docs/features/libraries.md) | 照片视频平台、外部文件扫描；部分元数据留在数据库，移动后重新扫描有身份关联限制 | 原图保留；已有照片直接浏览；外部变更刷新；身份与任务需执行前再核对 | 手机自动备份、账号、虚拟相册、地点地图与人脸识别不进入 MVP |
| [Immich 重复工具](https://github.com/immich-app/immich/blob/9ba71d908d2d90d9b9d51241e8c2091b7263be6e/docs/docs/features/duplicates-utility.md) | 机器学习识别视觉相似资产，人工选择保留与回收 | 先支持字节完全相同的重复文件，列明证据并人工处理 | 视觉相似不能等同于完全重复；感知相似识别不进入首期 |
| [PhotoPrism](https://github.com/photoprism/photoprism/blob/2758bb68ed107a2800e49cf6b85221c885cd9731/README.md) | 照片和视频浏览、RAW、过滤、内容标签 | 相机格式预览、拍摄信息和类型筛选 | 地图、人脸、自动标签体系和会员能力不照搬 |
| [Photoview](https://github.com/photoview/photoview/blob/30f99f6f824fb0d9d6e6f22374edd26ad79aba79/README.md)、[Home Gallery](https://github.com/xemle/home-gallery/blob/0272ea7d04133d9c02ad6fa93c41b83db654084d/README.md) | 围绕已有媒体来源浏览；Home Gallery 描述离线来源及内容身份 | 原件与预览分开，离线磁盘标记不可用，内容哈希用于准确去重 | 离线仅显示缓存不意味着仍可下载原件；不能把离线当成空目录 |
| [Paperless-ngx 用法](https://github.com/paperless-ngx/paperless-ngx/blob/c86c822bc7fb3b5a16dcdd5abe7c725b8ce913c7/docs/usage.md) | 文档 OCR、全文检索、标签和文档类型；消费目录中的文件被移入受管理结构 | PDF 预览、正文提取、扫描件 OCR、可解释的分类依据 | 不采用消费目录搬迁和标签中心模型；上传后原件保留在用户选择的目录 |
| [Docspell](https://github.com/eikek/docspell/blob/b0006733c13e6e7a848a424af56dc3d92576674d/README.md)、[Papermerge](https://github.com/papermerge/papermerge-core/blob/1f0c8c4682aa6a9f1f0ad185874b23aba0a9b907/README.md) | 文档管理、扫描与检索相关能力 | 文档提取失败可见，扫描件与文本 PDF 区分处理 | 邮件收件、企业流程和文档版本体系不进入 MVP |
| [Paperless AI](https://github.com/clusterzx/paperless-ai/blob/d2fda6991e5e55d51d6473997176bb8d054f61fd/README.md) | 基于模型的文档元数据分析和检索；README 明示当前仓库暂停维护 | 模型可配置，分类依据可读，内容分析有范围 | 仅参考产品行为，不预设为实施依赖；不引入完整 RAG 平台 |
| [Paperless GPT](https://github.com/icereed/paperless-gpt/blob/1dba575f35d80866edc9cb1ccda0637806ae1eaf/README.md) | 标题与标签建议、人工调整、OCR、多种模型与 OCR 服务 | 提取与建议分开、建议可编辑、服务失败不改原件 | 不制作新的可搜索 PDF 原件、不做复杂文档关系推断 |
| [GenAI File Organizer](https://github.com/jazmy/genai-file-organizer/blob/a73a7149fa59f57151e6349b46bab229e632d004/README.md) | 内容提取、分类、命名、验证、预览执行、历史与撤销、本地模型 | 分析→操作方案→人工审批→执行→历史；模型错误隔离 | 不引入多个判断模型、音视频转录、全类型深度分析 |
| [AIFiles](https://github.com/jjuliano/aifiles/blob/c1bcdacc9089f120ea4b90aa1d65c4f98b84c6cf/README.md) | 可配置命名模板、多模型、文件监听、审核与自动整理 | 中文自然语言指令、目录与文件名建议、可调整目标位置 | 用户要求人工审批，因此不采用自动执行和监听即整理 |
| [AI File Organizer](https://github.com/youi2000jp4/ai-file-organizer/blob/5d5b47406306a23a05b36575af08a71c61590974/README.md) | 自然语言、dry-run、源与目标清单、结构化输出、同名后缀 | 必须存在明确操作清单，审核的是具体路径而不是一句“同意整理” | CLI 的跳过确认能力不进入 FileSage |
| [Smart File Organizer](https://github.com/jagas32/smart-file-organizer/blob/17530fcc65f948147408faf752b7445d0995e3f8/README.md) | 方案先行、确认、复制校验、SHA-256 去重和阶段进度 | 执行前校验、准确重复证据、可追踪进度 | 示例流水线和演示结果不能用作 FileSage 的性能或准确率保证 |
| [CrispSorter](https://github.com/CrispStrobe/CrispSorter/blob/a142496ead324b47c83faaa31a5bb7b12f91a04f/README.md) | 内容提取、网格审核、修改后接受、批量操作、持久任务 | 逐项审批、可编辑建议、提取状态、断点历史 | 多模态后台、PDF 编辑、文献关系等会显著扩大首期范围 |
| [ClutterCut](https://github.com/caiowilson/ClutterCut/blob/889a3fea493317b1c4c5d3c4af3b05b9dea0570b/README.md)、[organize](https://github.com/tfeldmann/organize/blob/36a54572488d89dc9279d79848ecc067b632f1a5/README.md) | ClutterCut 描述确定性规则优先、预览和撤销；organize 提供规则整理 | 拍摄日期和哈希不交给模型猜测；语义分析与文件操作分开 | 不引入通用规则 DSL 或 RAG；Shell 另按根目录边界与逐项审批设计；ClutterCut 有许可条件 |
| [Czkawka](https://github.com/qarmin/czkawka/blob/eb8b91dbb4d25dff416202674ee0de84ee4b5e5c/README.md)、[rmlint](https://github.com/sahib/rmlint/blob/3a44e632b1b902effb6349a5e7e9378b920bd0a5/README.rst)、[fdupes](https://github.com/adrianlopezroche/fdupes/blob/a7c1da4c561a9f37c7f27bf2350404d209c86014/README.md) | 重复检测与文件清理专项能力 | 大小筛选后内容校验，不以文件名判断重复 | 不扩大到系统清理、坏文件批量删除或相似照片自动移除 |
| [Uppy](https://github.com/transloadit/uppy/blob/2f8e49b3aadcb2224bd83d33e77a24fdcf1bb5bc/README.md)、[tusd](https://github.com/tus/tusd/blob/78cc2291823e171b20d915570e3b68f554eab908/README.md) | 浏览器上传组件、可恢复上传服务 | 续传能力属于 MVP；失败项可重试，传输和发布状态分开 | 此处是行为与协议参考，没有预先锁定必须采用这些依赖 |
| [ExifTool](https://github.com/exiftool/exiftool/blob/2200871d9cef988051d2a99d67df3bda6cbb30a8/README)、[PDF.js](https://github.com/mozilla/pdf.js/blob/288892aadaa27f03ed0cd4724edf678c6bda8e61/README.md)、[Marked](https://github.com/markedjs/marked/blob/6dd5b3c892d5dd06046b8a52c1cefb3335613fea/README.md) | 元数据提取、PDF 查看、Markdown 解析专项工具 | 日期证据、PDF 分页查看、Markdown 渲染 | 解析成功与格式安全需要独立验证；Markdown 解析不等于 HTML 消毒 |

## 全部项目如何使用

下表为选题定位，详细功能仍以各仓库原文为准；所有项目均可从 [项目索引](catalog.md) 进入固定提交和上游来源链接。

| 类别 | 项目集合 | 对本项目的价值 |
| --- | --- | --- |
| 文件管理 | File Browser、Quantum、Copyparty、Filestash、NextExplorer、Dufs、h5ai、Gossa、gohttpserver、Cloudreve、AList、OpenList、KodExplorer、kodbox、SFTPGo、WebDAV、The File Browser、goDrive | 直接竞争界面和文件流程；云盘聚合、协议服务器与在线协作产品只取相关部分 |
| 个人云与同步 | Nextcloud、ownCloud、Pydio Cells、Seafile、Syncthing、rclone | 观察文件生命周期、后台任务、同步与外部修改边界；不复制平台规模 |
| 照片 | Immich、PhotoPrism、PiGallery2、Photoview、LibrePhotos、Lychee、Piwigo、Ente、Home Gallery、Sigal、Memories | 比较目录视图、媒体预览、日期信息、图库模型和重复审核 |
| 文档 | Paperless-ngx、Papermerge、Mayan EDMS、Teedy、Docspell、Stirling PDF | 区分文件浏览、文档归档与文档编辑，选取预览及提取能力 |
| AI 与规则整理 | Paperless AI、Paperless GPT、GenAI File Organizer、AIFiles、CrispSorter、jagas32 Smart File Organizer、youi2000jp4 AI File Organizer、SysAdminDoc FileOrganizer、ShichengRao Smart File Organizer、ClutterCut、SeyiDan Smart File Organizer、organize | 从自然语言与内容提取走到具体方案，比较审批、失败处理、去重和撤销 |
| 实现参考 | Czkawka、rmlint、fdupes、ExifTool、Uppy、tusd、PDF.js、Marked、Quantum 文档仓库 | 专项行为与格式边界；不是九个完整 FileSage 竞品 |
| 技术参考 | EPUB.js、SheetJS、docxjs、PPTXjs、ONLYOFFICE DocumentServer／sdkjs、Collabora、LibreOffice、zip.js、libarchive／libarchivejs、CodeMirror、Monaco、DOMPurify、python-docx、python-pptx、ExcelJS、PptxGenJS、pypdf、Docling、Unstructured、Tesseract、Bubblewrap、NsJail、Sandbox Runtime | 新增阅读、编辑、归档及 Shell 能力的候选证据；集成要求见技术覆盖说明 |

其中 Quantum 文档仓库是主项目的资料来源，不算独立产品；同名 Smart File Organizer 按仓库所有者区分。Mayan EDMS 的 GitHub README 明示该仓库为不再维护的旧镜像、上游转至 GitLab；不以旧镜像内容断言现行产品能力。许可原文优先于 README 的宣传称谓，“公开可见”也不等于任意复用。

## 功能决策矩阵

“MVP”意味着所有用户确认的核心场景均可闭环，并不意味着完整复制每个竞品。

| 能力 | 决策 | 理由与边界 |
| --- | --- | --- |
| 既有目录与多磁盘 | MVP | 单一配置根目录包含真实挂载目录，原件与结构为准 |
| 复制、移动、重命名、批量操作 | MVP | 包括跨盘移动、同名自动改名、明确失败结果 |
| 删除与恢复 | MVP | 同盘回收站；永久删除仅由用户明确操作 |
| 文件夹、大文件与可恢复上传 | MVP | 满足照片迁移和各类文件；前后台可见状态；不暴露半文件 |
| 照片、视频、音频、PDF、Markdown、文本阅读 | MVP | 实际阅读，PDF 书签与 Markdown 标题导航；支持矩阵明确 |
| HEIC、常见 RAW 与办公文件 | MVP 有界支持 | Office 包含工作表切换、DOCX 标题导航、PPT 逐页播放；前端只读，不承诺完美还原 |
| EPUB、归档与文本编辑 | MVP | 章节阅读、包内条目浏览、压缩／解压、文本及 Markdown 编辑保存 |
| 搜索与筛选 | MVP | 文件名与路径、类型、大小和日期；正文提取用于 AI，不自动扩大成全文搜索平台 |
| 文档 OCR 与内容分类 | MVP 有界支持 | 用户主动分析；文本先提取、扫描件本地 OCR；不改原 PDF |
| Agent 全部文件操作与逐项审批 | MVP | 根目录内包含内容修改、覆盖、永久删除、权限／属主、链接与 Shell；审批绑定具体影响与文件状态 |
| 审计与恢复 | MVP 有界支持 | 无旧内容备份；仅能恢复当前状态允许的可逆操作和回收项，内容覆盖及永久删除不可恢复 |
| 字节相同去重 | MVP | 内容校验证据，人工选择删除方式；链接别名不误报独立副本 |
| 视觉相似、人脸、自然语言找图、向量搜索 | 暂不纳入 | 独立模型、索引和误判成本；完全重复已经可闭环 |
| 自动备份、同步、外链、多人、在线 Office 编辑 | 暂不纳入 | 用户当前场景不需要，不能因竞品拥有就加入 |
| 软链接与 Shell | MVP | 仅根目录内链接，Shell 明列命令和具体文件影响；统一审批和并发提交 |
| 远程存储、存储池、RAID | 暂不纳入 | 保持原生文件管理，不引入额外存储模型 |

## 必须消除的含糊点

“尽量完善”落实为操作成功、冲突、取消、部分失败和不可用状态均有确定行为。“速度优先”落实为在明确测试环境中测量传输，并保持完整性和续传，而不是照搬竞品宣传吞吐。“各种文件”落实为任意普通文件可存储、支持格式有实际阅读能力。“Agent 全部权限”落实为根目录内全部文件操作，写入经过具体审批，不扩展到服务器系统管理。

日期归档使用 EXIF 等可信元数据；缺失时标记未知，不让模型猜日期。重复识别使用内容校验；模型仅解释或建议保留哪份。PDF 分类优先使用现有文件夹及用户指令，降低碎片化分类；不设没有评测依据的准确率承诺。

## 采集与核验说明

GitHub 公共 API 在采集时无剩余额度，使用 Git 固定提交和 raw 文件地址完成采集。检索发现 NextExplorer 和 Memories 的正确仓库分别是 `nxzai/NextExplorer`、`pulsejet/memories`；错误候选记录保留于检索说明，错误候选目录清除，正式索引仅保留正确仓库。

检索关键词包括 self-hosted file manager、resumable upload、directory-first photo gallery、AI file organizer dry-run、paperless AI manual review。先寻找项目官方仓库，再采集 README 关联文档，并对重点仓库读取目录树。完整性校验不代表功能实测或许可证法律审核。

检索入口与部分官方说明：[File Browser](https://github.com/filebrowser/filebrowser)、[Quantum](https://github.com/gtsteffaniak/filebrowser)、[NextExplorer](https://github.com/nxzai/NextExplorer)、[PiGallery2](https://github.com/bpatrik/pigallery2)、[Immich](https://github.com/immich-app/immich)、[Paperless-ngx](https://github.com/paperless-ngx/paperless-ngx)、[AI File Organizer](https://github.com/youi2000jp4/ai-file-organizer)。固定到采集提交的原文链接见各项目 `sources.json`。
