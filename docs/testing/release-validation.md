# v1.0.0 稳定版验证

**发布：**2026-10-24 · **提交：**`0239fd035dbb47e6e77e0cd735f88541fa1f4af1`

**Release：**[GitHub v1.0.0](https://github.com/int2t05/FileSage/releases/tag/v1.0.0) · [发布流水线](https://github.com/int2t05/FileSage/actions/runs/40102408176)

**主分支 CI：**[40101787216](https://github.com/int2t05/FileSage/actions/runs/40101787216) · **平台：**Linux amd64

## 验证结果

- **构建与安全：**Go 测试、Race、`vet`、govulncheck、Gitleaks v8.30.1 全历史扫描、前端审计、类型检查、构建和 Linux 浏览器 E2E 均通过。公开 `main` 的 166 个提交无 Gitleaks 命中。
- **发布包：**SHA-256 为 `036ed9bcc3512242a9ac73fdb6b93c18c3ab4206adfa1ee8809107a246d47ba2`；CI 完成包内冒烟测试，下载后本地和服务器校验通过。
- **服务器：**`filesage.service` 为 active，健康 API 返回 `ok`，重启计数为 0。二进制与 `web/index.html` 的 SHA-256 和发布包一致，Go 构建版本记录提交 `0239fd0`、工作区未修改。Agent 状态为 available，使用 `glm-5.3-flash`。
- **存储：**`/srv/filesage-files/home` 仍挂载于 `/dev/sda[/filesage-data]`（ext4）；部署保留原环境、数据目录和挂载配置。
- **上传：**隔离 Linux E2E 覆盖确认丢失与 PATCH 请求体中断；服务端偏移和最终文件 SHA-256 正确。持久服务上传／下载 E2E 通过。
- **可靠性：**限速负载 6 轮、24 个 1 GiB 文件，全部哈希一致。559 次 I/O 采样中 8 次超过 20%，峰值 53%，最长连续 2 次，未达到连续 3 次停止线。
- **文件与 Agent：**跨卷恢复、SQLite 完整性、备份恢复、会话续聊、计划审批、步骤执行、Shell 变化审阅和窄屏浏览器 E2E 通过。

## 已知边界

- 服务没有登录和 HTTPS，仅适用于可信局域网。
- 实体手机、用户真实文件样本及独立 Linux 主机恢复由后续人工验收；不阻塞 v1.0.0。
- 限速负载存在短时 I/O wait 峰值；未连续触发停止线。测试标准输出未保留，结论依据采样记录、文件校验、停止标记和服务状态。
- 已合并的 GitHub PR #9 仍保留旧提交只读引用；其中旧内网地址不含凭据，当前 `main` 已无该地址。GitHub 对 PR 缓存的处理范围见[官方说明](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/removing-sensitive-data-from-a-repository)。
