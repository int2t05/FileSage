# Agent 对话 API

对话和消息保存在 SQLite。系统支持单个 UTF-8 文本／Markdown 文件重命名或内容编辑、多文件文本／Markdown 内容分类、新建文件夹、复制普通文件或不含软链接与特殊文件的文件夹、移动普通文件、单条软链接、修改权限或属主、只读重复文件扫描，以及按拍摄日期归档照片。普通写入方案最多包含 20 项互不依赖的操作；照片归档单独生成最多 256 项的方案。重命名、内容编辑和内容分类均须用户确认后才读取正文并发送给配置模型。其余操作只使用任务描述、路径、固定索引元数据和明确给出的目标值。重复扫描经用户确认后作为独立后台任务运行：候选文件的完整内容只在 FileSage 服务器本地计算 SHA-256，不发送给模型，不修改文件，也不生成删除方案。除重复扫描外，确认分析只生成写入方案；所有写入都先经用户审计计划，再按计划固定的权限模式执行。

内置工具以稳定的动作 ID 注册，声明用途、输入契约、执行策略、校验器和执行器。Bash（API 动作 `run_shell`）与文件操作复用同一计划步骤、审批模式和审计记录；其隔离暂存、命令限制和差异复核仍由 Shell 执行器负责。Shell 原始输出只在隔离执行期间限量保存在内存中，不写入数据库或 API 响应；历史仅记录退出码和是否省略过输出。不保存完整旧文件正文。新工具必须由代码显式登记并复用计划执行流程；当前不支持运行时动态加载插件或在执行中临时生成依赖步骤。

## 模型配置

服务端使用环境变量配置 OpenAI-compatible Chat Completions；不写入 SQLite，也不会由 API 返回或记录密钥。

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| `FILESAGE_AGENT_BASE_URL` | 是 | 兼容 API 根地址，例如 Ollama `http://127.0.0.1:11434/v1`；远端必须使用 HTTPS，本机回环地址可使用 HTTP。服务自动请求 `/chat/completions`。 |
| `FILESAGE_AGENT_MODEL` | 是 | 请求使用的模型名。 |
| `FILESAGE_AGENT_PROVIDER` | 否 | 展示用服务标识，默认 `openai-compatible`。 |
| `FILESAGE_AGENT_API_KEY` | 否 | 仅作为服务端 Bearer 凭据发送；本地 Ollama 可留空。 |

配置缺失或无效时，文件服务照常运行；状态 API 返回 `available: false` 和不含凭据的原因。

## 接口

所有路径以 `/api/v1` 为前缀。POST 请求遵循现有同源校验；请求 JSON 不接受未知字段，单条消息最多 4 KiB，模型请求和响应都有大小及超时限制。

每个会话最多保存 100 条消息；分析文件正文上限为 256 KiB。模型澄清阶段最多接收 64 KiB 对话消息总量，超过任一限制时不会读取或发送文件正文。

### `GET /agent/status`

返回模型连接配置状态，不执行探测请求、不返回地址或密钥。`available: true` 只表示配置齐备，不代表模型已在线；首次模型请求才验证连接。

```json
{"available":true,"provider":"ollama","model":"qwen3:8b","destination":"local"}
```

不可用时包含 `reason`，取值为 `not_configured`、`incomplete_configuration` 或 `invalid_configuration`。

### `POST /agent/conversations`

创建会话并处理首条消息。

```json
{"message":"请根据 report.txt 内容给它改一个清晰的名称"}
```

返回 `201` 和会话对象。模型可以继续澄清（`state: "clarifying"`）、给出待确认分析卡（`state: "awaiting_confirmation"`），或明确说明暂不支持（`state: "unsupported"`）。模型暂时不可用时，会话仍会保存为 `failed`；只要会话保存成功，接口仍返回 `201`，响应不包含上游正文或密钥。

### `GET /agent/conversations/{id}`

读取已持久化会话、消息、当前分析卡和关联的任务／方案／重复扫描 ID；客户端保存会话 ID 后可在刷新或服务重启后恢复。不会读取文件或调用模型。

### `GET /agent/conversations?limit=20&offset=0`

返回按更新时间倒序排列的有界会话摘要，默认每页 20 条、最多 100 条。标题来自待确认目标或固定状态文案，不含对话消息正文或文件正文；有扫描时附带 `duplicateScanId`。

```json
{
  "conversations":[
    {"id":"会话 ID","state":"awaiting_approval","title":"依据正文标题重命名","updatedAt":"2026-10-15T08:30:00Z","taskId":"任务 ID","taskStatus":"succeeded","planId":"方案 ID","duplicateScanId":"扫描 ID"}
  ],
  "limit":20,
  "offset":0,
  "hasMore":false
}
```

### `POST /agent/conversations/{id}/messages`

在 `clarifying`、`awaiting_confirmation`、`unsupported` 或 `failed` 状态追加一条用户消息并继续对话。新消息会清除旧分析卡；`processing`、`analyzing` 和已生成方案的会话不可追加消息。用户消息持久化成功后返回 `200`，即使模型失败也会返回 `state: "failed"` 和安全错误码。

```json
{"message":"只处理管理目录内的这个文件，依据正文标题命名"}
```

### `POST /agent/conversations/{id}/confirm-analysis`

空请求体。仅当前状态为 `awaiting_confirmation` 时开始分析。重复确认在分析中返回 `409`；方案已生成后重复确认返回同一方案，不再次读取文件或创建方案。确认 `find_duplicates` 后会返回关联的 `duplicateScanId` 和 `analyzing` 状态，扫描在后台运行；其他确认操作开始后若模型或文件检查失败，会保存 `state: "failed"` 和安全错误码并返回 `200`。请求未能开始或未能持久化状态时才返回 HTTP 错误。

待确认卡示例：

```json
{
  "action":"rename",
  "goal":"依据正文标题规范文件名",
  "filePath":"文档/report.txt",
  "targetPath":"",
  "dataTypes":["UTF-8 文件正文全文"],
  "provider":"ollama",
  "model":"qwen3:8b",
  "destination":"local"
}
```

只读重复扫描卡使用以下形式；`filePath` 为空表示整个管理根目录：

```json
{
  "action":"find_duplicates",
  "goal":"找出资料中的重复文件",
  "filePath":"资料",
  "targetPath":"",
  "dataTypes":["任务描述与扫描范围会发送给模型；文件内容仅在 FileSage 服务器本地计算 SHA-256，不发送给模型"],
  "provider":"ollama",
  "model":"qwen3:8b",
  "destination":"local"
}
```

多项确认卡使用有序 `operations` 数组；单项卡同时保留 `action`、`filePath`、`targetPath` 和 `requested*` 标量字段以兼容现有客户端。每项沿用下方动作对应的路径与目标字段，例如：

```json
{
  "goal":"按年份建立照片归档目录",
  "operations":[
    {"action":"create_directory","filePath":"","targetPath":"照片/2024"},
    {"action":"create_directory","filePath":"","targetPath":"照片/2025"}
  ],
  "dataTypes":["任务描述、操作路径与目标值"],
  "provider":"ollama",
  "model":"qwen3:8b",
  "destination":"local"
}
```

卡片的 `action` 为 `rename`、`edit_text`、`create_directory`、`copy_file`、`copy_directory`、`move_file`、`create_symlink`、`set_permissions`、`set_owner`、`find_duplicates`、`archive_photos` 或 `classify_documents`、`create_file`、`overwrite_file`、`create_archive`、`extract_archive`、`trash`、`permanent_delete` 或 `run_shell`。`rename` 只支持管理根目录内单个 UTF-8 `.txt`、`.md`、`.markdown` 或 `.mdown` 普通文件；用户确认后，目标由正文分析给出。当用户已明确给出目标文件名或路径时，使用 `move_file` 表示直接改名或移动，不读取正文。`edit_text` 也只支持单个 UTF-8 文本／Markdown 文件，生成的候选作为计划步骤展示；用户审阅整份计划后，按计划审核时固定的执行模式写入。新建文件夹卡提供完整 `targetPath`，其父目录必须已存在。`copy_file`、`copy_directory` 和 `move_file` 同时提供 `filePath` 源路径与 `targetPath` 目标路径；目标父目录须已存在且目标路径须空缺。文件复制／移动的来源须为普通文件；目录复制不接受含软链接或特殊文件的来源，目标不能位于来源目录内。`create_symlink` 只允许单项操作：`filePath` 指向管理根目录内已存在的普通文件或目录，`targetPath` 是链接完整路径；链接父目录须已存在且不能经过软链接，目录链接不得指向自身或其子目录。同名目标由服务端自动编号，并在确认卡与审批项中展示最终路径；用户审阅整份计划后，按固定执行模式运行。权限和属主卡提供 `filePath`，`targetPath` 必须为空；`set_permissions` 还须提供 `requestedMode`，`set_owner` 须提供至少一个数字 `requestedOwnerUid`／`requestedGroupGid`。权限值范围为 `0..0777`，按 JSON 十进制传输（八进制 `0755` 应传 `493`）；UID／GID 为非负 32 位整数，不能从名称推断。元数据操作只支持普通文件和目录，拒绝软链接、特殊文件及带 setuid、setgid 或 sticky 位的文件；请求值不包含这些特殊位。它们的 `dataTypes` 仅列任务描述、路径和目标元数据。`find_duplicates` 的 `filePath` 是空值或管理根目录内的相对文件／目录路径，`targetPath` 必须为空；确认后启动只读扫描，不创建写入方案。扫描不跟随软链接，管理范围内的硬链接按设备号／inode 合并；仅完整成功的扫描可读取分组结果，取消、文件变化、资源上限或服务重启均不发布部分结果。`archive_photos` 的 `filePath` 是空值或管理根目录内的来源目录，`targetPath` 是已存在的真实目标目录。确认后只读取固定完整索引中的图片路径和拍摄日期，不读取或发送图片正文；目标子树从来源扫描中排除，没有拍摄日期的图片跳过。扫描上限为 10,000 条索引记录，最多生成 256 项；任一上限超出时整批拒绝。目标按 `YYYY/MM` 组织，冲突名称在方案中自动编号且不覆盖。服务端返回一张专用单项确认卡和 `operations` 中唯一一条 `archive_photos` 项；确认分析仅生成方案；用户审阅整份计划后，bypasspermission 模式按序执行，手动模式等待用户逐步启动。执行前状态冲突会使方案失效，不自动重试或覆盖。
`classify_documents` 只允许作为确认卡中的唯一操作。模型给出来源 `filePath`（空字符串代表管理根目录）和已存在类别根目录 `targetPath`；服务端扫描后在唯一操作项中补入候选 `files` 与 `categories`，模型不能提供或改写这些清单。类别必须是类别根目录下的真实直接子目录，不跟随软链接；来源扫描覆盖类别根目录时会排除该子树。扫描最多访问 10,000 个目录项、发现 20 个候选文件和 50 个类别。候选限 UTF-8 `.txt`、`.md`、`.markdown`、`.mdown`、`.pdf`、`.docx`、`.pptx` 普通文件；文本单件最多 64 KiB、合计最多 256 KiB，PDF／Office 单件最多 16 MiB、总输入最多 64 MiB。确认卡展示准确路径、文件大小和类别目录，并为每个候选绑定不含正文的文件状态版本；无候选或超限时不读取正文并拒绝创建确认卡。

点击“确认并分析正文”后，服务端复核文件版本，以 no-follow 方式读取候选并抽取正文；仅在所有候选正文都成功提取且合计不超过 256 KiB 后才发送给模型。PDF／Office 使用本机工具，最多读取每份前 20 页；Office 先经 Bubblewrap 隔离的 LibreOffice 转为 PDF，再提取文字。扫描件 OCR 默认关闭；用户可在确认卡主动启用，每批最多处理 2 份 PDF／Office，仅识别整份无文字层文档的前 20 页。混合文字与扫描页暂不逐页补识别；未提取到文字或任一文件变化时整批不建方案。模型只能返回每个文件编号、类别编号（`-1` 表示跳过）和固定理由代码（`topic`、`title`、`date`、`document_type`、`other`）；遗漏、重复、越界编号或自由文本理由会使整批失败。每个成功分类项生成一条移动操作，绑定源文件与目标类别目录快照；同名目标自动编号且不覆盖。用户审阅整份计划后按固定执行模式运行。正文不会写入 SQLite、对话历史、审计或应用日志。

确认卡示例（候选清单由服务端生成）：

```json
{
  "action":"classify_documents",
  "goal":"按正文主题分类文档",
  "filePath":"",
  "targetPath":"文档/类别",
  "operations":[
    {
      "action":"classify_documents",
      "filePath":"",
      "targetPath":"文档/类别",
      "files":[{"path":"文档/合同.md","sizeBytes":412,"version":"<64 个十六进制字符的状态令牌>"}],
      "categories":[{"id":0,"path":"文档/类别/合同"}]
    }
  ],
  "dataTypes":["确认后提取候选文本、PDF 或 Office 正文并发送给模型；每份 PDF／Office 最多读取前 20 页；扫描件 OCR 默认关闭，用户可主动启用，每批最多 2 份 PDF／Office"],
  "provider":"openai-compatible",
  "model":"glm-5.3-flash",
  "destination":"local"
}
```

目录、越界路径以及其他操作均不得伪装为受支持任务。

### 重复文件扫描状态与结果

确认 `find_duplicates` 后，会话对象返回 `duplicateScanId`。状态接口及分页结果仅返回进度、相对路径、大小和修改时间，不返回文件正文或 SHA-256 摘要。扫描先枚举范围内目录项，再完整读取相同大小的候选文件并逐个计算 SHA-256，最后重新枚举并比较范围内的路径及文件状态；若末轮复核发现新增、删除或状态变化，扫描会失败并丢弃全部结果。成功表示首轮与末轮采集到的目录项状态一致，不是文件系统原子快照，也不代表结果之后不会变化；任何后续写操作仍须重新核验当前文件状态。结果只保存在服务端 SQLite 中。

`GET /agent/duplicate-scans/{id}` 返回扫描状态：

```json
{"id":"扫描 ID","status":"running","phase":"hashing","scopePath":"资料","entriesVisited":1200,"candidateFiles":24,"filesHashed":8,"bytesHashed":1048576,"totalBytes":3145728,"groupCount":0,"duplicateFileCount":0,"reclaimableBytes":0,"errorCode":"","updatedAt":"2026-10-16T08:30:00Z"}
```

`status` 为 `queued`、`running`、`succeeded`、`failed` 或 `cancelled`；`phase` 为 `enumerating`、`hashing` 或 `complete`，哈希阶段还包含末尾目录项复核。仅 `succeeded` 状态可以访问 `GET /agent/duplicate-scans/{id}/groups?limit=20&offset=0` 和 `GET /agent/duplicate-scans/{id}/groups/{groupId}/files?limit=100&offset=0`。两者 `limit` 最大为 100、`offset` 最大为 100000，按页返回；组 ID 使用十进制字符串。`reclaimableBytes` 是假设每组只保留一份时的空间估算；扫描本身不选择要保留的副本，也不删除或修改任何文件。

组列表返回 `groups`、`limit`、`offset`、`hasMore`；每组包含字符串 `id`、单个文件 `size` 和 `duplicateCount`。组内文件列表返回 `files` 及相同分页字段；每个文件只包含相对 `path`、`size` 和 `modifiedAt`。

`POST /agent/duplicate-scans/{id}/cancel` 接受空请求体并请求停止扫描。取消、失败或进程重启时不展示部分分组；查找不存在的 ID 返回 `404`，未成功扫描的分组请求返回 `409`。取消竞争以服务端最终状态为准，客户端应重新读取状态。

多项确认仅接受 `create_directory`、`copy_file`、`copy_directory`、`move_file`、`set_permissions` 和 `set_owner`，不得包含 `rename` 或 `edit_text`。服务端先校验整组操作、目标依赖和路径重叠，再一次性绑定每项文件快照并创建方案；任一项无效或文件状态冲突时，不会留下部分方案。方案保持数组顺序。用户审计整份计划后，bypasspermission 开启时按序执行；关闭时只允许执行计划中的下一步，其他步骤保持待执行。首个失败或冲突会停止后续项。服务重启只核实已完成／在途操作；未开始步骤按计划审核时固定的模式继续或等待用户启动；不自动重放不确定操作。

重启后，若重命名源仍匹配快照且目标空缺，可在方案版本递增后重新确认；若源已消失且目标存在，执行前内容摘要未持久化，状态会转为 `partial` 并要求人工核查，不能仅凭文件身份判成功。多项方案恢复到待继续状态时也会递增 `planVersion`，旧版本审批或执行请求返回 `409`。

重命名确认后校验正文大小与 UTF-8，再请求模型；不保存正文。重命名分析响应只接受 `supported`、`targetName` 和受限枚举 `reasonCode`；服务端将枚举映射为固定说明，不接受模型自由文本理由、证据或回复写入方案。新建方案只能来自用户确认，旧手动提案创建接口已移除；计划审阅后由统一计划接口启动，手动模式每次只运行一个步骤。移动绑定源文件快照与目标父目录身份，冲突时拒绝执行，跨挂载未能移除源文件时记录部分完成。元数据方案在创建、审核和执行前均校验完整文件快照，并以设备号／inode 串行化别名写入；恢复时仅在状态恰好仍为原值时允许用户重试，恰好符合目标值时标记成功，其他状态均要求人工核查，不自动重放。

Agent 方案 API 只返回明确允许展示的字段，不暴露内部文件快照。`set_permissions` 操作返回十进制 `beforeMode` 与目标 `mode`；`set_owner` 返回 `beforeOwnerUid`、`ownerUid`、`beforeGroupGid`、`groupGid`。未请求改变的 UID／GID 在目标字段中返回原值，供审批界面明确展示完整前后状态。普通文件内容和旧内容备份均不包含在方案或审计中。

### 计划审核与执行

`GET /settings/agent` 返回持久化的 `{ "bypassPermission": true }`；`PUT /settings/agent` 接收相同结构，设置只影响之后审核的计划。`GET /agent/plans/{planId}` 在计划审核后另返回 `executionMode`（`bypass` 或 `manual`）与 `reviewedAt`。

`POST /agent/plans/{planId}/start` 接收 `{"expectedPlanVersion":1,"expectedExecutionMode":"bypass"}`。客户端应提交其刚读取并向用户展示的模式；服务端在同一事务内比较当前设置并保存计划审核状态。模式或方案版本已变化时返回 `409`，不审核、不执行。bypass 模式下审核成功后自动顺序执行；manual 模式只确认整份计划，文件操作尚未执行。两个模式下计划审核都是必需门禁。

manual 模式使用 `POST /agent/plans/{planId}/steps/{operationId}/execute` 和 `{"expectedPlanVersion":1}`，只运行计划中的下一项；非下一项、模式错误或方案版本变化返回 `409`。成功响应是当前完整方案。执行期间客户端可轮询 `GET /agent/plans/{planId}`，逐项读取 `executionStatus` 和 `resultMessage`。`stopRequested` 表示已收到停止请求；`POST /agent/plans/{planId}/stop` 接收 `{"expectedPlanVersion":1}`，当前操作安全结束后停止后续项。已完成的文件操作不会回滚，未开始的步骤保持未执行。服务重启后核查已完成／在途操作，不自动重放不确定操作。

计划响应包含稳定工具动作 ID；未知工具仍按通用步骤展示，后端不允许未注册的动作执行。Shell 操作返回 `exitCode`，若执行产生过原始输出，则返回 `outputDiscarded: true`；不返回 `stdout` 或 `stderr`。Shell 文件变化作为单个工具步骤的详情呈现，不再逐文件审批；计划审计后由受控 Shell 执行器统一提交并逐项记录结果。

## 会话对象

```json
{
  "id":"会话 ID",
  "state":"awaiting_confirmation",
  "messages":[{"role":"user","content":"..."},{"role":"assistant","content":"..."}],
  "analysisCard": {"action":"rename","goal":"...","filePath":"文档/report.txt","targetPath":"","dataTypes":["UTF-8 文件正文全文"],"provider":"ollama","model":"qwen3:8b","destination":"local"},
  "taskId":"",
  "planId":"",
  "duplicateScanId":"",
  "error":""
}
```

状态为 `clarifying`、`awaiting_confirmation`、`unsupported`、`failed`、`processing`、`analyzing`、`analysis_complete` 或 `awaiting_approval`。`taskId`／`planId` 在方案持久化后提供；`duplicateScanId` 在重复扫描创建后提供，可用于查询扫描状态和结果。开始新一轮对话时会清除当前会话对旧扫描的关联，原扫描任务及结果仍保留在任务历史中。`error` 只返回不含上游信息的错误代码。

## 错误

| HTTP | 语义 |
| --- | --- |
| `400` | JSON、消息或会话 ID 无效；确认接口请求体非空 |
| `404` | 会话、扫描或分组不存在 |
| `409` | 会话状态不允许操作、重复分析正在进行、确认前文件状态不符合安全要求，或扫描尚未成功而请求读取结果 |
| `413` | 请求或待分析文本超过限制 |
| `503` | 对话服务未初始化；模型未配置会保存为 `failed` 会话，不返回此状态 |
| `500` | 会话或方案无法持久化，不能据此认为消息已保存 |

一旦用户消息或分析状态已持久化，接口使用成功状态码并返回会话对象；模型／文件分析失败通过安全错误码和会话状态表达，避免客户端误以为消息未保存而重复提交。上游模型失败不会作为 `502` 返回。

模型请求禁用 HTTP 重定向；服务端错误和审计不记录 API 密钥、上游响应正文或文件正文。

## 兼容性依据

请求遵循 Chat Completions 的 `model`、`messages`、非流式 JSON 响应模式；Ollama 官方兼容接口示例见 [OpenAI compatibility](https://github.com/ollama/ollama/blob/main/docs/api/openai-compatibility.mdx)，JSON 模式定义见 [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat/object)。
