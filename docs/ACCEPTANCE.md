# 验收记录

**截至：** 2026-10-24

这里只记录实际验证结果及适用范围；整体状态和下一步见[项目进度](IMPLEMENTATION_PLAN.md)。当前候选位于本地 `main`，工作区干净，尚未推送或部署。

## 已验证

| 范围 | 结果 |
| --- | --- |
| GitHub 主线 | `b484215` 的 [CI 通过](https://github.com/int2t05/FileSage/actions/runs/39955724932)，包含 Go、Race、`go vet`、漏洞扫描、Web E2E 和 Linux 包冒烟。 |
| 本地候选 | Go 全量测试、`go vet`、构建和安全扫描通过；Web 273 项测试、两项 TypeScript 检查、生产构建通过。Windows 浏览器 E2E 5 项通过、2 项 Linux 专项跳过。 |
| Linux 隔离验证 | Go 全量测试、`go vet`、构建通过；候选快照 `5dfb722` 的 `cmd/filesage`、`internal/httpapi`、`internal/upload`、`internal/origincheck` Race 检查通过。Linux 浏览器 E2E 7/7 通过，Agent E2E 连续两轮 8/8。 |
| Linux 发布包 | 本轮脚本防误操作调整前，工作区快照在服务器隔离目录构建 amd64 包；13 个 Go 模块许可、SHA-256 和启动／写入／重启／备份／恢复／读取冒烟通过。新增的端口占用和 `dist` 符号链接保护仅通过 Bash 语法检查，相关场景未动态复测。 |
| 写请求来源校验 | API 与 TUS 写请求共用来源检查；跨站请求被拒绝。对活动上传的跨站 PATCH、DELETE 测试确认暂存内容不变。 |
| 测试服务器 | 持久 `filesage.service` 运行 `4600241`，监听 `192.0.2.172:8080`，健康检查通过、重启数 0；unit 与仓库模板一致，服务器 drop-in 声明第二块 ext4 子卷，两个环境文件均为 `root:root 0600`。SQLite v30 完整性与外键检查通过；QA 副本和 `/tmp` 顶层 FileSage 临时目录均已清理。 |
| 文件与传输 | 20 GiB 上传重启续传两次、20 GiB 下载、72 GiB 并发上传均完成哈希核对；双盘恢复验证通过；删除竞态重复 20 次通过。 |

## 尚未验收

- 本地候选的 GitHub CI、候选部署及部署后端到端复验尚未完成。
- 持续负载测试在 18/24 轮触发 I/O 停止线；须换环境或限速后补完 6 轮、1 小时运行和真实断网续传哈希核对。
- 尚未在独立 Linux 主机和独立介质完成异机恢复。
- 尚未用用户真实文件验收；实体手机由用户自行测试。
- GitHub 尚无稳定版 Release。

测试服务器的 npm 审计请求因 TLS 重置未完成；同一锁文件在本机审计为 0 项漏洞。FileSage 没有用户认证或 HTTPS，同源校验也不提供身份认证；仅应在本机或可信局域网使用，详见[部署说明](DEPLOYMENT.md)。
