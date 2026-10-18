# FileSage 技术需求覆盖与验证依据

资料日期：2026-10-15。对应 [成熟需求基线](../docs/PRD.md)。本覆盖表引用专项 GitHub 技术仓库和官方手册；本地 Context7 查询快照不随公开仓库发布。扩展后的候选选型见 [技术方案](../docs/TECH.md)。固定提交、来源和校验值见 [索引](catalog.md)、[仓库清单](manifest.json) 和 [核验报告](verification.md)。

本文将产品能力映射到实现参考、官方依据和待验证的集成条件。技术方向与 ADR 由助手在用户授权下确定，具体状态见[技术方案](../docs/TECH.md)；候选项目未作为 FileSage 运行组件验证，引用也不表示功能已经实现。

## 1. 需求对应的技术资料

| 产品需求 | 已采集的证据与候选 | 技术方案必须解决及验证 |
| --- | --- | --- |
| 原生目录浏览、搜索、批量管理 | [Quantum](https://github.com/gtsteffaniak/filebrowser/blob/91bc608f0ec6a3246c29e5fc87acdd46a5fadf44/README.md)、[NextExplorer](https://github.com/nxzai/NextExplorer/blob/662fb42986689f0cc14b634e603b56ff674e3471/README.md)、[goDrive](https://github.com/MrCodeEU/goDrive/blob/54d7af59be21b02d73868fab4fc3cbdb8f4caeb9/README.md) | 实际文件系统与索引一致；分页、外部修改刷新、目录与子项重叠操作 |
| 高速上传与可靠续传 | [Copyparty](https://github.com/9001/copyparty/blob/0dc1cb329120743ca85599ba3fba24849f137677/README.md)、[Uppy](https://github.com/transloadit/uppy/blob/2f8e49b3aadcb2224bd83d33e77a24fdcf1bb5bc/README.md)、[tusd](https://github.com/tus/tusd/blob/78cc2291823e171b20d915570e3b68f554eab908/README.md) | 大文件流式传输、批量小文件、续传身份、原子发布、吞吐与完整性；不照搬宣传速度 |
| 照片与 HEIC／RAW | [PhotoPrism](https://github.com/photoprism/photoprism/blob/2758bb68ed107a2800e49cf6b85221c885cd9731/README.md)、[Immich 格式](https://github.com/immich-app/immich/blob/9ba71d908d2d90d9b9d51241e8c2091b7263be6e/docs/docs/features/supported-formats.md)、[ExifTool](https://github.com/exiftool/exiftool/blob/2200871d9cef988051d2a99d67df3bda6cbb30a8/README) | 真实手机／相机样本、方向与日期、预览生成、缓存失效，不能只凭扩展名保证支持 |
| 音视频播放 | [Copyparty](https://github.com/9001/copyparty/blob/0dc1cb329120743ca85599ba3fba24849f137677/README.md)、[goDrive 预览](https://github.com/MrCodeEU/goDrive/blob/54d7af59be21b02d73868fab4fc3cbdb8f4caeb9/docs/features/previews.md) | 浏览器编解码差异、分段请求、拖动与缩略图；不引入全格式转码平台 |
| PDF 分页、书签与查找 | [PDF.js](https://github.com/mozilla/pdf.js/blob/288892aadaa27f03ed0cd4724edf678c6bda8e61/README.md)、[pypdf](https://github.com/py-pdf/pypdf/blob/f2f158a2b051818f4981b34ff15d1ca67b324611/README.md) | PDF.js 负责阅读；pypdf 作提取／修改候选；扫描件文字与数字 PDF 区分，加密／损坏有错误 |
| Markdown 渲染、标题导航 | [Marked](https://github.com/markedjs/marked/blob/6dd5b3c892d5dd06046b8a52c1cefb3335613fea/README.md)、[DOMPurify](https://github.com/cure53/DOMPurify/blob/cde95990b2190338cfd7c00b768f3d81d3219d61/README.md) | 标题树、相对资源、消毒顺序；解析器不等于内容安全隔离，禁止文件内脚本 |
| 文本／Markdown 编辑 | [CodeMirror 指南](https://github.com/codemirror/website/blob/84be9d74034d6db925b6834c9e39ab2b438c002c/site/docs/guide/index.md)、[Monaco](https://github.com/microsoft/monaco-editor/blob/f54d769519ccf7d00e534346b47d01502afcf257/README.md) | 编辑草稿、保存状态、编码、文件变化检测；编辑器内撤销不等于服务端旧内容备份 |
| DOCX 阅读和标题目录 | [docxjs](https://github.com/VolodymyrBaydalka/docxjs/blob/c533f383069008d4a1e05c6171ef8777fd078385/README.md)、[python-docx](https://github.com/python-openxml/python-docx/blob/e45454602b53e8e572b179ccf1c91093ec9f4ed7/README.md)、[LibreOffice 转换](https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html) | docxjs 的 HTML 渲染有版式限制且不完整支持 Word 目录域；需另取标题结构或评估转换阅读路径 |
| Excel 工作表切换 | [SheetJS](https://github.com/SheetJS/sheetjs/blob/3f44ddd99c704d7ca35ea76fef7b8af2d2d435cb/README.md)、[当前官方 HTML 文档](https://docs.sheetjs.com/docs/api/utilities/html/)、[ExcelJS](https://github.com/exceljs/exceljs/blob/5bed18b45e824f409b08456b59b87430ded023ab/README.md) | 工作表数据与名称、合并单元格、缓存公式值、按需显示；社区版数据渲染不等于完整 Excel 排版或公式计算 |
| PPTX 逐页播放 | [PPTXjs](https://github.com/meshesha/PPTXjs/blob/1a9260b2062f89ba822aeb54da792236780af8e7/README.md)、[LibreOffice](https://github.com/LibreOffice/core/blob/d72fcb5ff2e7a1c8101aae1f522908e03f833075/README.md)、[转换过滤器](https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html) | HTML 渲染与服务端转换两条候选；真实样本的图表、字体、页面和全屏；不保证完整动画 |
| Office 综合阅读候选 | [ONLYOFFICE DocumentServer](https://github.com/ONLYOFFICE/DocumentServer/tree/ef39256f5f058ca8d24ec2bd2cd3ac834b0793e7)、[sdkjs](https://github.com/ONLYOFFICE/sdkjs/tree/72b0421c0bbf9d01eed9cf14834ae47eb2df1b50)、[Collabora](https://github.com/CollaboraOnline/online/blob/b76eddb352f05faa6896e8600f401b2ba13beb26/README.md) | 作为备选能力参考；评估部署资源、离线依赖和许可，不能因阅读需求直接带入完整协作平台 |
| EPUB 章节与阅读位置 | [EPUB.js](https://github.com/futurepress/epub.js/blob/eee359d0790002115a1156a9833c54f4bcd44c1d/README.md) | EPUB 布局差异、章节定位、阅读位置及内容变化后失效；禁用书内脚本和外部资源 |
| 压缩包目录与单条目阅读 | [libarchive](https://github.com/libarchive/libarchive/blob/76d41c18cf53c888afb2c9477fd595a5d3984192/README.md)、[libarchivejs](https://github.com/nika-begiashvili/libarchivejs/blob/0989c1a6db20d030d793b1763e20d880068091bd/README.md)、[zip.js](https://github.com/gildas-lormeau/zip.js/blob/4d6a9fe65d7c5eacd59c9db7962287aa063f0080/README.md) | 条目枚举与按需提取、密码／损坏、大条目限制、包内阅读器复用；不把整包强制解压到用户目录 |
| 压缩／解压 | 同上三个项目 | 格式矩阵分读取与写入；RAR 不能因“可读”推断“可创建”；防越界、软链接绕过及解压资源耗尽 |
| 文档提取与中英文 OCR | [Docling](https://github.com/docling-project/docling/blob/6fadbae317b09c4dd4051c0f1fc1746c6baf04bc/README.md)、[Unstructured](https://github.com/Unstructured-IO/unstructured/blob/0b7f806e0f6ef540df22fad5627756a72663eb11/README.md)、[Tesseract](https://github.com/tesseract-ocr/tesseract/blob/b2544450ca01407259ed53c870b7e5e4c67f7789/README.md)、[Paperless](https://github.com/paperless-ngx/paperless-ngx/blob/c86c822bc7fb3b5a16dcdd5abe7c725b8ce913c7/docs/usage.md) | 提取范围与页码、中英文模型、真实扫描样本；不能自动把整个文档分析平台引入首期 |
| Agent 内容修改 | [python-docx](https://github.com/python-openxml/python-docx/blob/e45454602b53e8e572b179ccf1c91093ec9f4ed7/README.md)、[python-pptx](https://github.com/scanny/python-pptx/blob/278b47b1dedd5b46ee84c286e77cdfb0bf4594be/README.rst)、[ExcelJS](https://github.com/exceljs/exceljs/blob/5bed18b45e824f409b08456b59b87430ded023ab/README.md)、[pypdf](https://github.com/py-pdf/pypdf/blob/f2f158a2b051818f4981b34ff15d1ca67b324611/README.md) | 文件级读写与格式级修改分开；测试修改后的可读性和未修改部分；这些库不是通用保真编辑器 |
| Agent 新文件与演示文稿生成 | [PptxGenJS](https://github.com/gitbrent/PptxGenJS/blob/3c9ec1b687c174952166f6a34b5e87ebf69fa469/README.md)、上述格式库 | 新生成与修改既有文件能力不同，不用能生成新文稿来证明能无损编辑已有文稿 |
| 准确去重 | [Czkawka](https://github.com/qarmin/czkawka/blob/eb8b91dbb4d25dff416202674ee0de84ee4b5e5c/README.md)、[fdupes](https://github.com/adrianlopezroche/fdupes/blob/a7c1da4c561a9f37c7f27bf2350404d209c86014/README.md) | 大小筛选和内容确认；软／硬链接别名不能报成独立副本；不把视觉相似当作完全重复 |
| 权限、属主和软链接 | [chmod](https://man7.org/linux/man-pages/man2/chmod.2.html)、[chown](https://man7.org/linux/man-pages/man2/chown.2.html)、[软链接语义](https://man7.org/linux/man-pages/man7/symlink.7.html)、[路径解析](https://man7.org/linux/man-pages/man2/openat2.2.html) | 运行用户能力、权限不足、链接自身与目标、循环及替换；“根目录内全部操作”不等于授予服务器系统管理权限 |
| Shell 根目录边界 | [Bubblewrap](https://github.com/containers/bubblewrap/blob/1fb0aa26d3eb66254f7742e704add7f82f41a582/README.md)、[NsJail](https://github.com/google/nsjail/blob/4ff54a6e0d5b65a0e1633d6e2fd1425ba6c882ce/README.md)、[Sandbox Runtime](https://github.com/anthropics/sandbox-runtime/blob/9a277e5e04af1ab1b04feea4400a6ed8a5b72039/README.md)、[Landlock](https://man7.org/linux/man-pages/man7/landlock.7.html) | 根目录外文件隔离、整个进程树、权限／属主变更需求与沙箱兼容性；沙箱本身不负责审批，也不负责所有并发提交 |
| 逐项审批与审计 | [GenAI File Organizer](https://github.com/jazmy/genai-file-organizer/blob/a73a7149fa59f57151e6349b46bab229e632d004/README.md)、[AI File Organizer](https://github.com/youi2000jp4/ai-file-organizer/blob/5d5b47406306a23a05b36575af08a71c61590974/README.md)、[CrispSorter](https://github.com/CrispStrobe/CrispSorter/blob/a142496ead324b47c83faaa31a5bb7b12f91a04f/README.md) | 方案与批准绑定、内容差异、权限前后值、具体 Shell 影响、过期审批、执行结果；不引入旧内容备份 |
| 阅读／编辑与 Agent 并发 | [Linux rename](https://man7.org/linux/man-pages/man2/rename.2.html)、[HTTP 条件请求](https://www.rfc-editor.org/rfc/rfc9110.html)、[CodeMirror 指南](https://github.com/codemirror/website/blob/84be9d74034d6db925b6834c9e39ab2b438c002c/site/docs/guide/index.md) | 状态校验、条件提交、单文件写协调、草稿保留、路径别名和目录冲突；这是集成设计要求，不能由一个解析库包办 |
| 回收、故障和历史 | [NextExplorer 回收站](https://github.com/nxzai/NextExplorer/blob/662fb42986689f0cc14b634e603b56ff674e3471/docs/admin/trash.md)、[goDrive](https://github.com/MrCodeEU/goDrive/blob/54d7af59be21b02d73868fab4fc3cbdb8f4caeb9/README.md) | 普通删除可恢复，永久删除及覆盖不可恢复；历史不包含内容备份；故障不能重放已完成写操作 |

## 2. 并发行为设计

产品需要同时支持浏览、上传、手动编辑和 Agent 执行。推荐的技术方案方向是状态校验与受协调提交：读取完整状态，写入先准备结果，提交时检查源状态及目标冲突。同一对象的所有写入口，包括 Shell，走同一协调机制；软链接、硬链接和父目录变更必须参与对象识别。

前端已有未保存草稿时，收到文件更新只提示，不替换草稿。保存冲突时重新加载或另存为；Agent 旧审批遇到文件变化必须失效。读取不能返回半写内容，衍生预览绑定文件状态，结果过期即标记或重建。该行为不需要保存历史内容备份，审计也不得暗中保存可还原旧文件的全文或差异。

Shell 审批列出影响目标，只检查命令字符串或只做根目录沙箱都不足以保证“仅执行所批文件修改”。技术方案须评估受控工作区准备结果和统一提交，或等价的明确文件影响约束；任意直接写原件的命令不得绕过并发检查。无法确定影响范围的 Shell 任务拒绝执行。

单次重命名的原子性质不能推广为跨文件系统移动或跨目录批量事务。网络层条件请求也不能约束直接在服务器上运行的外部工具；应用必须检测外部变化并展示冲突，不声称锁住所有外部程序。

## 3. 需要完成的专项验证

1. Office 阅读样本：DOCX 标题／分页／图片／表格，XLSX 多工作表／合并单元格／已有公式值，PPTX 图文／图表／中文字体／逐页播放。纯浏览器解析和服务端转换都应评估，选择满足需求的最简单组合。
2. EPUB 和归档：章节导航、阅读位置、非默认布局；ZIP／7z／RAR／TAR／TAR.GZ 条目浏览、单条目读取与解压，损坏、密码、越界和链接条目。
3. 修改审批：文本差异、格式文件修改结果、覆盖、新建、权限／属主、链接自身与目标、具体 Shell 影响和永久删除。无需历史内容副本，保存行为不能暗中建立版本系统。
4. 并发：Agent 修改阅读中的文件、修改未保存编辑目标、两个写任务、同名上传、目录与子项、软／硬链接别名、外部工具替换，以及批准后长时间运行的 Shell。
5. 传输与资源：真实大文件、小文件批次、中断和重启；同时运行 OCR、转换与 Agent，测量吞吐、首屏延迟、峰值内存和数据完整性。

这些是技术方案和开发验收工作，没有执行过专项验证，不能将资料核验结果作为功能验收。

## 4. 上游资料状态与复用边界

SheetJS 的 GitHub README 明确指向新的主仓库，因此现行能力判断以 [官方文档](https://docs.sheetjs.com/docs/api/utilities/html/) 为准，GitHub 文件只作为快照。LibreOffice 的 GitHub 仓库是只读镜像，转换接口依据 [官方帮助](https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html)。

docxjs 明示 HTML 渲染和目录域的限制；PPTXjs 是一条候选渲染路径，未以真实样本验证保真度。ONLYOFFICE／Collabora 参考不意味着需要引入协作编辑或多套服务。Sandbox Runtime 的上游定位为研究预览，Bubblewrap 明示策略取决于使用者配置，均不能替代应用侧的审批和文件提交设计。

自动仓库检索最初误用 `python-openxml/python-pptx`；已修正为官方 `scanny/python-pptx`，正式清单只保留正确仓库。保留上游许可证，技术方案实施复用前需核对所选组件的具体许可、分发方式和依赖，不以公开仓库可访问性判断可自由复制。

## 5. 技术选型增补

补充资料用于比较实际集成方式；只有技术方案和 ADR 中记录的选项属于已选方向。引用项目不表示依赖已经引入或 FileSage 已完成原型。

| 需求／决策 | 新增本地参考 | 对照结论与缺口 |
| --- | --- | --- |
| Go、Python、Rust 后端 | [FastAPI](https://github.com/fastapi/fastapi/blob/49a09a0d4e876b4e7d6d87fc049710263aaeaa7e/README.md)、[Axum](https://github.com/tokio-rs/axum/blob/e57b547eb95de74b73b34719dd5926254a085634/README.md)；Go `os.Root` 以官方文档为准 | 已选 Go 模块化单体（ADR-0001）；开发效率和性能尚未测量 |
| Go 路径安全 | [Google safeopen](https://github.com/google/safeopen/blob/43626d6f468560fc611ad54f1f17f3a455dbe92d/README.md)、[Go traversal-resistant API](https://go.dev/blog/osroot)、[Go 安全修复公告](https://pkg.go.dev/vuln/GO-2026-4970) | `os.Root` 是路径边界 API，不替代版本补丁审查。已发生的漏洞说明要按当前安全版本构建并测试末尾斜杠、软链接、并发替换等情况 |
| SQLite 状态库与搜索 | [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3/blob/dbbe931904f2ee06546448fbe7cf7780f61bc110/README.md)、[modernc SQLite](https://github.com/modernc-org/sqlite/blob/0fc3f90d9e280a7356d8609520bc2ba081bfc248/README.md)、[SQLite WAL](https://www.sqlite.org/wal.html)、[FTS5](https://www.sqlite.org/fts5.html) | 单机读写并行但单写者；不能将 WAL 数据库放到网络文件系统；索引可重建，任务审批需持久化 |
| 外部文件变化 | [fsnotify](https://github.com/fsnotify/fsnotify/blob/20b1e15ef3c70caeb37ea2bd184f48ef8382669e/README.md)、[Linux inotify](https://man7.org/linux/man-pages/man7/inotify.7.html) | 通知只做增量更新；事件溢出、挂载／卸载和服务重启都须重扫，文件系统仍是唯一事实来源 |
| 上传协议 | [tus-js-client](https://github.com/tus/tus-js-client/blob/0fee69e35750090a6f7f9ea30abf08852f0253ca/README.md)、[tus 协议快照](https://tus.io/protocols/resumable-upload.html) | tus 只提供可续传传输状态；目标卷安全发布、数字后缀、校验与跨文件系统移动仍由 FileSage 协调 |
| 后台任务 | [Asynq](https://github.com/hibiken/asynq/blob/d135f1439bee74e989b7f9b41ecd542cc87f024a/README.md) | Redis 队列作为复杂度对照；当前单实例建议 SQLite 持久任务表和有界进程内 worker，重启后按阶段和文件状态恢复 |
| 前端框架 | [Vue](https://github.com/vuejs/core/blob/bf5f0dd877dd3c23ce85c2cbc1dd771567e84c8c/README.md)、[React](https://github.com/facebook/react/blob/99c106c43439ad7af05d42f66582d8d05190e9ff/README.md)、[Vite](https://github.com/vitejs/vite/blob/c2c9ef4dcd05b3e4d88844ea3a13dc23819ca2a5/README.md) | 已选 React 与 Vite（ADR-0007）；具体依赖版本以 `web/package.json` 为准 |
| 本地／远程模型 | [Ollama](https://github.com/ollama/ollama/blob/8e4dad49f9f527762610fd419ee2ae39207f67a4/README.md)、[llama.cpp](https://github.com/ggml-org/llama.cpp/blob/3e390af18de1131dda27913c00511725b4274c8a/README.md) | 以服务端适配器统一成结构化操作清单；模型调用不可成为文件权限层，密钥和数据范围需独立管控 |
| Shell 沙箱 | [Bubblewrap](https://github.com/containers/bubblewrap/blob/1fb0aa26d3eb66254f7742e704add7f82f41a582/README.md)、[NsJail](https://github.com/google/nsjail/blob/4ff54a6e0d5b65a0e1633d6e2fd1425ba6c882ce/README.md)、[Landlock](https://man7.org/linux/man-pages/man7/landlock.7.html) | namespace／文件访问沙箱不等同于审批和写入协调；暂存区差异方案未经目标主机实测，不能直接宣称安全 |
| 媒体／归档 | [libvips](https://github.com/libvips/libvips/blob/06564b0a4c3f473d0e0cb9f9977ab647888aa43d/README.md)、[mholt/archives](https://github.com/mholt/archives/blob/9bf66b8e091c2318c7de7b2054c19ddf7b2702ce/README.md)、既有 libarchive 与 ExifTool | 不同编解码和压缩格式需按 PRD 样本矩阵逐项验收，支持名录或 README 不能证明保真和抗恶意样本能力 |
| 无认证浏览器端点 | [OWASP CSRF 资料](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html) | 来源校验能降低跨站请求风险，不能限制同局域网设备；默认回环监听，局域网部署须显式绑定可信私有地址并配置主机防火墙和允许主机名 |

硬链接写入语义见 [ADR-0003](../docs/design/adr/0003-filesystem-write-safety.md)；决策记录不代替实现和 Linux 验证。路径替换可能打断其他别名，原地写入可能改变管理根目录外的未知别名。

官方网页 HTML 原文及可读文本仅保存在维护者本地，公开仓库只包含来源与哈希，见[官方来源清单](official/manifest.json)。Context7 快照仅用于本地补充检索；完整示例应回到固定提交或官方文档确认。网页与查询快照不冒充固定 Git 提交资料。
