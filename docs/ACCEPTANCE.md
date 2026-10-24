# 验收摘要

成熟稳定版 `v1.0.0` 已发布并部署。主分支与发布流水线通过 Go/Web CI、Race、漏洞扫描、Linux 浏览器 E2E 和发布包校验。

限速负载文件哈希全部一致，I/O wait 未连续触发停止线；上传确认丢失、请求体中断续传和跨盘恢复均已验证。实体手机、用户真实文件及独立主机恢复尚未验收。完整证据见[发布验证记录](testing/release-validation.md)。

2026-10-24 线上复核：健康检查、文件 API 和首页均返回 HTTP 200；Agent 可用。systemd 为 `active`、`enabled`，配置 `Restart=on-failure`，重启计数为 0；运行二进制和页面入口校验和与 v1.0.0 发布包一致。管理根目录位于 NVMe（约 390 GiB 可用）；`home` 直接挂载到 SSD `/dev/sda[/filesage-data]`（约 398 GiB 可用）。两块 ext4 文件系统独立管理，容量不合并；SSD 下的 8 个常用分类目录已通过文件 API 验证。
