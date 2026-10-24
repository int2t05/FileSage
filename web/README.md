# FileSage Web

React + TypeScript + Vite 前端。仓库运行方式、当前功能状态和安全说明见[根目录 README](../README.md)。

开发时在本目录运行 `npm ci` 和 `npm run dev`；Vite 会将 `/api` 请求代理到 `127.0.0.1:8080`。

运行 `npm run e2e` 可验证浏览器主流程。文件版本校验依赖 Linux 文件身份信息；非 Linux 平台会跳过相关用例，Linux CI 会运行完整流程。

上传确认丢失和分块续传用例可用 `FILESAGE_E2E_BASE_URL` 指向隔离的 Linux 测试服务，并分别用 `--grep "确认丢失"` 或 `--grep "分块确认"` 单独运行。外部服务模式不会自动启动或清理服务；只对一次性测试根目录使用。

测试单个 PATCH 中途断开时，先把隔离 Linux 服务通过 SSH 隧道映射到本机回环地址，再将 `FILESAGE_E2E_PROXY_UPSTREAM` 设为该地址并运行 `npm run e2e -- --grep "PATCH传输中途"`。故障代理只接受本机上游，在首个分块转发 8 MiB 后断开；服务和测试数据仍由调用方管理。
