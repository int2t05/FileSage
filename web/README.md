# FileSage Web

React + TypeScript + Vite 前端。仓库运行方式、当前功能状态和安全说明见[根目录 README](../README.md)。

开发时在本目录运行 `npm ci` 和 `npm run dev`；Vite 会将 `/api` 请求代理到 `127.0.0.1:8080`。

运行 `npm run e2e` 可验证浏览器主流程。文件版本校验依赖 Linux 文件身份信息；非 Linux 平台会跳过 Agent 改名执行用例，Linux CI 会运行完整流程。
