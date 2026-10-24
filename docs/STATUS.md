# 项目状态

**成熟度：**稳定版 `v1.0.0` 已发布；服务器运行 `main` 的 `3f51331`。

**提交：**`0239fd0` · [GitHub Release](https://github.com/int2t05/FileSage/releases/tag/v1.0.0)

**线上复核（2026-10-25）：**健康 API 正常；Agent 模型流式回复通过。systemd 为 `active`、`enabled`；测试会话已清空。

**存储：**`/srv/filesage-files` 位于 NVMe（约 390 GiB 可用）；`/srv/filesage-files/home` 直挂 SSD（约 398 GiB 可用）。两块盘独立；`home/` 下已建立 `Documents`、`Pictures`、`Videos`、`Music`、`Downloads`、`Archives`、`Code`、`Backups` 真实目录。

## 已完成

- Go/Web CI、Race、漏洞扫描、Linux E2E 与发布包冒烟通过。
- 上传中断续传、跨盘恢复及限速负载验证通过。
- `main` 的 166 个提交通过 Gitleaks 全历史扫描；文档和发布流程已整理。

## 非阻塞验收边界

- 用户自行验收实体手机和真实文件样本；不阻塞发布。
- 独立 Linux 主机恢复尚未验证；作为运维边界保留。

详细结果与已知风险见[验收摘要](ACCEPTANCE.md)和[发布验证记录](testing/release-validation.md)。
