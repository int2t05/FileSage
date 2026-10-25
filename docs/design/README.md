# 设计与实现决策

本目录记录长期有效的界面基线和架构取舍。产品范围见[PRD](../PRD.md)，当前技术结构见[技术基线](../TECH.md)；运行接口契约见[Agent API](../api/agent-conversation.md)与[归档 API](../api/archive.md)。

## 设计基线

- [UI/UX](UIUX.md)：信息架构、桌面与手机布局、关键流程、组件和图标约定。

## 架构决策

| 主题 | 决策记录 |
| --- | --- |
| 服务与元数据 | [Go 模块化单体](adr/0001-go-modular-monolith.md) · [SQLite 元数据](adr/0002-sqlite-metadata.md) |
| 文件与传输 | [文件写入安全](adr/0003-filesystem-write-safety.md) · [断点续传](adr/0004-resumable-upload.md) |
| 阅读与界面 | [阅读器渲染](adr/0005-reader-rendering.md) · [React 前端](adr/0007-frontend-framework.md) |
| Agent 与执行 | [Shell 暂存隔离](adr/0006-shell-staging-sandbox.md) · [文本编辑暂存](adr/0008-agent-text-edit-staging.md) · [工具步骤与会话](adr/0009-agent-tool-steps.md) · [模型工具调用与索引上下文](adr/0010-agent-tool-runtime.md) |

每份 ADR 说明问题、选项、采用理由、代价和验证依据；运行时行为以当前代码及 API／部署说明为准。
