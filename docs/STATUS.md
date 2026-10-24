# 项目状态

**阶段：**稳定版 `v1.0.0` 已发布并部署。

**提交：**`0239fd0` · [GitHub Release](https://github.com/int2t05/FileSage/releases/tag/v1.0.0)

**服务器：**服务健康；程序与前端文件校验和匹配发布包。

## 已完成

- Go/Web CI、Race、漏洞扫描、Linux E2E 与发布包冒烟通过。
- 上传中断续传、跨盘恢复及限速负载验证通过。
- `main` 的 166 个提交通过 Gitleaks 全历史扫描；文档和发布流程已整理。

## 后续

- 用户自行验收实体手机和真实文件样本；不阻塞发布。
- 独立 Linux 主机恢复尚未验证；作为运维边界保留。

详细结果与已知风险见[验收摘要](ACCEPTANCE.md)和[发布验证记录](testing/release-validation.md)。
