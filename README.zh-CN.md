# FileSage — Linux 自托管文件管理器

<p align="right"><img src="web/public/filesage.svg" width="64" alt="FileSage 图标" /></p>

[![CI](https://github.com/int2t05/FileSage/actions/workflows/ci.yml/badge.svg)](https://github.com/int2t05/FileSage/actions/workflows/ci.yml)
[![MIT License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

FileSage 是自托管 Linux 文件管理器，直接管理挂载磁盘上的真实目录，支持大文件续传和浏览器阅读。
Agent 通过自然语言澄清需求，并在你审阅整份计划后执行。默认开启
`bypasspermission`，按顺序执行；在设置中关闭后，每一步由你手动执行。

[English](README.md)

## 项目状态

稳定版通过 CI 验证后提供 Linux amd64 发布包。当前状态见[项目状态](docs/STATUS.md)、
[验收摘要](docs/ACCEPTANCE.md)和 [GitHub Releases](https://github.com/int2t05/FileSage/releases)。

## 能力

- 按原生目录浏览、搜索、筛选、上传和整理文件；可在管理根目录下挂载多块磁盘；
  文件夹就是类别。
- 支持续传（单文件与暂存总量默认各限 20 GiB，可配置）、原件下载，以及常见压缩包的浏览、预览、创建和解压。
- 在浏览器阅读常见图片、音视频、PDF、文本、Markdown、Office、表格、EPUB
  和压缩包；不支持或过于复杂的文件仍可下载。
- 与 OpenAI-compatible 模型对话以澄清任务，审阅整份计划后执行。同一会话可继续处理
  后续任务，刷新后也可从历史恢复。默认开启 `bypasspermission` 并按序运行；在设置中
  关闭后逐步启动。可展开步骤查看命令、退出码、限长输出和文件差异；回执用于同会话续聊。
- 使用 SQLite 保存任务和搜索元数据，数据目录与管理文件分开；文件系统始终是事实来源。

## 快速开始

需要 Linux、Go 1.27.3 或更新版本、Node.js 24 和 npm。Office 转换、OCR、
视频海报、7-Zip 归档等能力需要额外系统工具，详见[部署说明](docs/DEPLOYMENT.md)。

```sh
cd web
npm ci
npm run build
cd ..
go build -o ./filesage ./cmd/filesage

mkdir -p "$HOME/FileSageFiles" "$HOME/.local/state/filesage"
export FILESAGE_ROOT="$HOME/FileSageFiles"
export FILESAGE_DATA_DIR="$HOME/.local/state/filesage"
export FILESAGE_WEB_DIR="$(pwd)/web/dist"
export FILESAGE_LISTEN_ADDR="127.0.0.1:8080"
./filesage
```

打开 `http://127.0.0.1:8080`。`FILESAGE_ROOT` 必须预先存在；
`FILESAGE_DATA_DIR` 必须位于管理目录之外。默认只监听本机回环地址。若开放局域网访问，
只绑定可信私有网卡并配置防火墙。FileSage 没有登录或密码。

## 验证变更

```sh
go test -count=1 ./...
go vet ./...
go build ./...

cd web
npm ci
npm test
npm run typecheck
npm run build
npx playwright install chromium
npm run e2e
```

端到端测试使用本地模拟模型、临时文件和数据库。[GitHub Actions](.github/workflows/ci.yml) 还会运行 race 检查、漏洞扫描和 Linux 发布包冒烟测试。

## 项目文档

- [文档导航](docs/README.md) · [项目状态](docs/STATUS.md) · [验收记录](docs/ACCEPTANCE.md)
- [参与贡献](CONTRIBUTING.md) · [安全问题反馈](SECURITY.md)

## 反馈与支持

请通过 [GitHub Issues](https://github.com/int2t05/FileSage/issues/new/choose)
提交可复现的问题或功能建议。不要在公开内容中发布密钥、私人文件或可利用的漏洞细节；
私密安全问题请按[安全报告说明](SECURITY.md)提交。

## 安全边界

FileSage 面向可信个人网络，不提供用户认证或 HTTPS 终止。能访问服务的网络客户端即可浏览和
修改管理文件、调整 Agent 审批模式、创建并审批任务，也可消耗已配置模型的调用额度。
仅在本机或可信局域网使用，不要暴露到公网或不可信网络。
Agent 对话消息会随每轮交流发送给已配置模型；文件分析只在你确认分析卡后
读取并发送正文。确认 Shell 分析卡后，stdout/stderr 每路最多 16 KiB 并保存到本地任务记录。
下一轮会将确认过的命令、范围、工作目录、执行状态、限长输出和文件变化路径、类型与状态作为不可信证据发送给配置模型。FileSage 不会为 Shell 另行读取文件正文；命令输出可能包含文件内容或其他敏感信息。确认前请查看卡片列出的数据范围，具体边界见
[Agent API 说明](docs/api/agent-conversation.md)。

## 许可证

FileSage 原创代码和文档采用 [MIT 许可证](LICENSE)。第三方资源与依赖沿用各自条款，
详见[第三方许可说明](THIRD_PARTY_NOTICES.md)。公开仓库发布研究总结、来源链接和采集工具；
本地抓取的上游文档不随仓库发布。
