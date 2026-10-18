# ADR-0001：Go 模块化单体

## 状态

**已决定；Go/Linux 构建与路径边界回归通过，正式安全公告复核和目标服务部署仍待完成。**

## 背景

FileSage 是单 Linux 主机上的个人服务，权限边界围绕原生文件系统，需处理大文件、并发读写、可靠上传和长时间任务。当前目录只有 PRD 与调研资料，没有应用代码、既有技术栈或可据以判断的团队熟悉度。

## 建议决策

使用 Go 实现一个模块化单体进程。文件访问、上传、索引、任务、Agent 审批和 API 分为内部模块；转换、OCR、媒体和归档工具按任务以受限子进程调用。Go 1.27.2 为最低版本，构建与部署持续采用当时受支持且修复安全公告的补丁版本；Go `os.Root` 覆盖已公开的软链接路径安全问题，并由越界链接回归测试约束。

## 备选方案

| 方案 | 适配点 | 主要代价／拒绝理由 |
| --- | --- | --- |
| Python + FastAPI | OCR、Office、文档解析生态直接；Paperless／Docling 可参考 | 文件安全边界需要专用安全打开方式和额外封装；Python 运行时、依赖与打包面增加 |
| Go + Python 常驻工作服务 | 两种生态各取所长 | 双运行时、进程间协议和资源故障面；当前未证明需要常驻解析服务 |
| Rust + Axum | 内存安全和异步服务能力 | 维护／开发熟悉度未知，外部解析工具仍需调用；没有项目实证说明其足以抵消成本 |

## 影响

正面：单一部署单元，应用级并发和取消控制集中；Go 有根目录文件 API，文件服务与上传参考可直接检查。负面：复杂 Office 和 OCR 仍依赖本机外部工具；Go 标准库不自动提供 FileSage 所需的权限策略、并发语义和恢复逻辑。若安全补丁不可用或关键格式原型失败，应重新评估 Python／混合方案。

## 证据

- [Go `os.Root` 官方说明](https://go.dev/blog/osroot)（访问 2026-10-15）：Go 1.24 起支持目录范围 API，并讨论路径遍历和 TOCTOU。
- [Go 官方版本史](https://go.dev/doc/devel/release)（访问 2026-10-15）：调研时发布记录列出 Go 1.27.2。
- [Go 官方漏洞 GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970)（访问 2026-10-15）：披露过 `os.Root` 末尾软链接路径问题，说明必须按补丁版本和实际路径验证。
- [tusd](https://github.com/tus/tusd/blob/78cc2291823e171b20d915570e3b68f554eab908/README.md)、[Quantum](https://github.com/gtsteffaniak/filebrowser/blob/91bc608f0ec6a3246c29e5fc87acdd46a5fadf44/README.md)（固定提交和文件哈希见 reference 清单）：可参考 Go 在本地文件服务／续传场景的使用；不能证明 FileSage 已达目标。
- [FastAPI](https://github.com/fastapi/fastapi/blob/49a09a0d4e876b4e7d6d87fc049710263aaeaa7e/README.md)、[Axum](https://github.com/tokio-rs/axum/blob/e57b547eb95de74b73b34719dd5926254a085634/README.md)（固定提交见清单）：框架候选参考，未运行比较。

## 决策记录

2026-10-15：用户授权助手决定 Proposed ADR。本 ADR 选定 Go 模块化单体，最低版本为 Go 1.27.2；这是技术负责人作出的决定，不是声称用户逐项审计了该 ADR。实现通过官方版本漏洞巡检、Linux 多挂载点与越界链接测试后，才能升级为成熟状态。
