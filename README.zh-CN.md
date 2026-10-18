# FileSage

<p align="right"><img src="web/public/filesage.svg" width="64" alt="FileSage 图标" /></p>

[![CI](https://github.com/int2t05/FileSage/actions/workflows/ci.yml/badge.svg)](https://github.com/int2t05/FileSage/actions/workflows/ci.yml)
[![MIT License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

面向 Linux 的本地优先文件工作台。FileSage 直接管理挂载磁盘上的真实目录；
Agent 提出的文件变更须经你审阅、审批和确认后才执行。

[English](README.md)

## 项目状态

MVP 开发中，尚无稳定版或预编译安装包。已验证能力和待验项见
[验收台账](docs/ACCEPTANCE.md)。

## 能力

- 按原生目录浏览、搜索、筛选、上传和整理文件；可在管理根目录下挂载多块磁盘；
  文件夹就是类别。
- 支持续传、原件下载，以及常见压缩包的浏览、预览、创建和解压。
- 在浏览器阅读常见图片、音视频、PDF、文本、Markdown、Office、表格、EPUB
  和压缩包；不支持或过于复杂的文件仍可下载。
- 使用 OpenAI-compatible 模型分析你明确确认的范围；逐项查看操作建议、审批、
  再次确认执行，并查阅审计记录。
- 使用 SQLite 保存任务和搜索元数据，数据目录与管理文件分开；文件系统始终是事实来源。

## 快速开始

需要 Linux、Go 1.27.2 或更新版本、Node.js 24 和 npm。Office 转换、OCR、
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
```

[GitHub Actions](.github/workflows/ci.yml) 会运行相同检查。

## 项目文档

- [部署与配置](docs/DEPLOYMENT.md)
- [产品需求](docs/PRD.md) · [技术决策与 ADR](docs/TECH.md)
- [UI/UX 设计](docs/design/UIUX.md) · [图标展示页](docs/design/icon-study.html) · [交互原型](docs/design/prototype/README.md)
- [功能验收与格式支持](docs/ACCEPTANCE.md)
- [竞品与实现调研](reference/README.md)
- [参与贡献](CONTRIBUTING.md) · [安全问题反馈](SECURITY.md)

## 反馈与支持

请通过 [GitHub Issues](https://github.com/int2t05/FileSage/issues/new/choose)
提交可复现的问题或功能建议。不要在公开内容中发布密钥、私人文件或可利用的漏洞细节；
私密安全问题请按[安全报告说明](SECURITY.md)提交。

## 安全边界

FileSage 面向可信个人网络，不提供用户认证、HTTPS 终止或公网防护。仅在本机或可信局域网
使用，并配置防火墙。Agent 只会在用户确认任务和数据范围后，把相应提示词或文件正文发送给
已配置模型；具体边界见 [Agent API 说明](docs/agent-conversation-api.md)。

## 许可证

FileSage 原创代码和文档采用 [MIT 许可证](LICENSE)。第三方资源与依赖沿用各自条款，
详见[第三方许可说明](THIRD_PARTY_NOTICES.md)。公开仓库发布研究总结、来源链接和采集工具；
本地抓取的上游文档不随仓库发布。
