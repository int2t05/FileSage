# FileSage GitHub 调研资料

初次调研：2026-10-14（Asia/Shanghai）。技术选型补充调研：2026-10-15（Asia/Shanghai）。

## 调研范围与方法

围绕原生文件系统管理、多磁盘、浏览器上传、照片与文档预览、AI 建议与人工审批，覆盖直接竞品、专项产品和实现参考。数量服务于需求覆盖，不以仓库热度代替适配性。

每个项目记录默认分支提交定位、README、许可证和相关文档来源；只按需取得资料，不执行上游代码，也不下载完整源码、依赖、模型或媒体。公开仓库保留研究总结、来源清单和采集工具；本地上游原文不随仓库发布。需要复核时可按固定提交重新采集。采集失败、缺失和分支回退记录在清单中，不把未抓到的资料视为功能不存在。

## 输出与验证

- `projects/`：本地按类别及仓库名保存上游资料、来源和 SHA-256；已加入 `.gitignore`，不会随公开仓库发布。
- `manifest.json`：公开的仓库、固定提交、上游来源、文件哈希和失败记录；不包含第三方原文。
- `catalog.md`：链接至固定提交的项目索引。
- `research.md`：竞品分析、功能提炼、取舍与证据边界。
- `technical-coverage.md`：阅读、编辑、归档和 Agent 执行等需求对应的技术参考与验证缺口。
- `context7/`：本地 EPUB、工作表阅读与文本编辑的文档查询快照，仅作补充证据；不随仓库发布。
- `official/`：公开来源、采集时间和哈希清单；HTML 与正文快照仅保存在本地。
- `docs/TECH.md`：候选架构、选型对照、技术审计 ADR 和未决风险；前端 ADR-0007 已接受。
- `docs/design/UIUX.md`：已定稿的目录优先界面结构、响应式交互、可用性状态和组件基线。
- `docs/design/icon-study.html`：使用固定提交 SVG 构建的交互式图标规范页。
- `docs/design/prototype/`：React 桌面／手机交互原型及运行说明。
- `tools/collect.py`：入口文档采集与索引生成；仅使用 Python 标准库与 Git。
- `tools/deepen.py`：重点项目固定提交目录树与相关文档补充。
- `tools/collect_icons.py`、`tools/build_icon_page.py`：采集并本地构建图标样例页。
- `tools/verify.py`、`verification.md`：文件完整性与自写文档链接核验。
- `../docs/PRD.md`：已确认的成熟需求基线。

首批技术资料覆盖文件管理、媒体、文档、Agent 分类和阅读组件。补充调研增加后端框架、SQLite 驱动、文件变化监听、任务队列、前端框架、图标库、组件库、本地模型服务和媒体／归档工具，以支持[技术选型审计](../docs/TECH.md)及 [UI/UX 审计](../docs/design/UIUX.md)。所有候选仍须结合 FileSage 的单用户、Linux、多挂载点及原生目录约束判断。

本地核验包括：候选仓库可访问性、README 成功率、采集文件校验值和来源、关键竞品原文阅读、文档内部链接。公开仓库中的[核验报告](verification.md)记录本地采集批次结果；第三方原文未随仓库分发。该调研不等同于运行竞品验证；README 的声明和实测能力必须区分。

上游资料保留其原始许可证；研究结论是 FileSage 的需求判断，不表示获准复制上游实现。

运行方式：在项目根目录使用 `python -B reference/tools/collect.py` 采集入口资料，再运行 `python -B reference/tools/deepen.py` 补充重点项目文档，使用 `python -B reference/tools/collect_official.py` 补充官方网页，最后使用 `python -B reference/tools/verify.py` 核验。已有成功仓库快照会复用，不自动追踪上游最新提交；如需新的调研日期，应创建独立批次。
