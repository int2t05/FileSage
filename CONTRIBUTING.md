# 参与贡献

感谢你帮助改进 FileSage。小型修复可以直接提交 Pull Request；较大的功能或架构变更，请先开 issue 讨论范围和验收方式。

## 本地验证

后端：

```sh
find cmd internal test -type f -name '*.go' -print0 | xargs -0 gofmt -w
go test -count=1 ./...
go vet ./...
go build ./...
```

前端：

```sh
cd web
npm ci
npm test
npm run typecheck
npm run build
```

## Pull Request

- 说明问题、改动和用户可见行为。
- 列出实际运行的验证命令及结果；界面改动附桌面和窄屏截图。
- 不提交真实个人文件、服务地址、API 密钥、数据库或本地测试数据。
- 文件写入须遵循根目录边界、版本冲突检查和显式审批流程。
- 代码注释保持简短；新注释使用简体中文，并解释必要约束或原因。

FileSage 使用 [MIT 许可证](LICENSE)。提交变更即表示你有权按该许可证授权所贡献内容。
