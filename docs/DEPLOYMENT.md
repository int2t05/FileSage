# Linux 部署

FileSage 直接读写服务器原生文件系统。首次部署先使用独立测试目录验证路径、挂载和账户权限，再指向个人数据。

## 安装 Release 包

Release 包适用于 Linux amd64，不需要在服务器安装 Go 或 Node.js。从 [GitHub Releases](https://github.com/int2t05/FileSage/releases) 下载 tar 包和校验文件，放在同一目录后先校验再解压：

```sh
sha256sum -c filesage_vX.Y.Z_linux_amd64.tar.gz.sha256
tar -xzf filesage_vX.Y.Z_linux_amd64.tar.gz
cd filesage_vX.Y.Z_linux_amd64
```

包内含 `filesage`、预构建页面 `web/`、systemd 模板、环境示例、部署说明和许可证文件。推送已审阅的 `vMAJOR.MINOR.PATCH` 标签会触发构建和发布；当前仓库尚无稳定版 Release。

## 从源码构建

需要 Go 1.27.2 或更新版本、Node.js 24 和 npm：

```sh
cd web
npm ci
npm run build
cd ..
go build -o ./filesage ./cmd/filesage
```

## 运行环境

以下环境变量在 Go 服务启动时读取：

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `FILESAGE_ROOT` | 是 | 已存在的绝对路径；服务只能管理此目录树内文件。独立磁盘可挂载到该树下。 |
| `FILESAGE_DATA_DIR` | 是 | SQLite 和上传暂存目录；必须位于管理根目录之外。 |
| `FILESAGE_WEB_DIR` | 否 | 页面目录；源码构建时通常为 `web/dist`，systemd 示例使用 `/usr/local/lib/filesage/web`。设置后由 Go 同源托管页面和 API。 |
| `FILESAGE_LISTEN_ADDR` | 否 | 监听地址，默认 `127.0.0.1:8080`。只允许回环、私有或链路本地 IP。 |
| `FILESAGE_ALLOWED_HOSTS` | 否 | 逗号分隔的额外主机名／IP，不带端口。 |
| `FILESAGE_AGENT_BASE_URL` | 否 | OpenAI-compatible Chat Completions 根地址；远程地址须用 HTTPS。 |
| `FILESAGE_AGENT_MODEL` | 否 | Agent 模型名；不配置时基础文件管理仍可使用。 |
| `FILESAGE_AGENT_PROVIDER` | 否 | 展示用服务标识，默认 `openai-compatible`。 |
| `FILESAGE_AGENT_API_KEY` | 否 | 进程环境变量中的 Bearer 密钥；不写入数据库、日志或 API 响应。 |

最小运行示例：

```sh
mkdir -p "$HOME/FileSageFiles" "$HOME/.local/state/filesage"
export FILESAGE_ROOT="$HOME/FileSageFiles"
export FILESAGE_DATA_DIR="$HOME/.local/state/filesage"
export FILESAGE_WEB_DIR="$(pwd)/web/dist"
export FILESAGE_LISTEN_ADDR="127.0.0.1:8080"
./filesage
```

检查服务：

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/api/v1/health
```

## systemd 专用账户

仓库提供 [unit 模板](../deploy/filesage.service)和[环境文件示例](../deploy/filesage.env.example)。示例以 `filesage` 非 root 系统用户运行，SQLite 状态目录为 `/var/lib/filesage`，管理目录为 `/srv/filesage-files`。根据实际路径编辑 unit 和环境文件：

```sh
sudo useradd --system --user-group --home-dir /var/lib/filesage --no-create-home --shell /usr/sbin/nologin filesage
sudo install -d -o root -g root -m 0755 /usr/local/lib/filesage/web
sudo install -o root -g root -m 0755 ./filesage /usr/local/lib/filesage/filesage
sudo cp -R web/. /usr/local/lib/filesage/web/
sudo install -d -o root -g root -m 0755 /etc/filesage
sudo install -o root -g root -m 0600 deploy/filesage.env.example /etc/filesage/filesage.env
sudoedit /etc/filesage/filesage.env
```

从源码目录部署时，将复制命令中的 `web/` 改为 `web/dist/`。

只对专为 FileSage 新建的目录授予所有权：

```sh
sudo install -d -o filesage -g filesage -m 0750 /srv/filesage-files
```

已有或共享的数据卷应只为选定目录配置 ACL 或专用组权限。不要对整块卷递归 `chown`，也不要使用 `chmod -R 777`。服务账户需在每个管理卷上拥有目录遍历、读取和所需写入权限。

unit 的 `RequiresMountsFor` 必须与实际 `FILESAGE_ROOT` 和启动时必需的独立挂载相符；systemd 不会从 `EnvironmentFile` 展开目录变量。数据库目录也必须在管理根目录之外。

安装并校验 unit 后启动：

```sh
sudo install -o root -g root -m 0644 deploy/filesage.service /etc/systemd/system/filesage.service
sudo systemd-analyze verify /etc/systemd/system/filesage.service
sudo systemctl daemon-reload
sudo systemctl enable --now filesage.service
sudo systemctl status filesage.service
```

更改 unit 后先运行 `daemon-reload`。更改环境文件后重启服务。检查日志：`sudo journalctl -u filesage.service`。

## 更新版本

保持现有环境文件、管理根目录和数据目录不变。更新前停止服务，再替换二进制和 Web 文件；随后启动并检查健康接口与服务日志：

```sh
sudo systemctl stop filesage.service
sudo install -o root -g root -m 0755 ./filesage /usr/local/lib/filesage/filesage
sudo cp -R web/. /usr/local/lib/filesage/web/
sudo systemctl start filesage.service
curl --fail --silent --show-error http://127.0.0.1:8080/api/v1/health
sudo journalctl -u filesage.service -n 100 --no-pager
```

若 `FILESAGE_LISTEN_ADDR` 绑定了其他地址，健康检查应替换为对应地址。数据库迁移在服务启动时执行。迁移后的旧二进制可能不兼容新 schema；若启动失败，应保留日志并修复前向版本，不要直接回退旧二进制。v27 会清除历史 Shell 输出，并从 SQLite WAL 中截断旧页。

从源码目录更新时，将复制命令中的 `web/` 改为 `web/dist/`。

## 可选工具

能力缺失时对应功能会明确不可用。发行版安装包名称可能不同。

| 工具 | 功能 |
| --- | --- |
| `ffmpeg` | 视频海报 |
| `heif-dec` 或 `heif-convert`、`dcraw_emu`、`prlimit` | HEIF／HEIC 与 RAW 图片预览 |
| `7z`、`7zz` 或 `7za` | 7z／RAR 浏览，7z 归档创建 |
| LibreOffice、Bubblewrap（`bwrap`）、`prlimit` | PPTX 预览；Bubblewrap 和 systemd cgroup v2 委托也用于 Agent Shell |
| Poppler `pdftotext`、`pdftoppm`、`pdfinfo` | Office／PDF 正文提取与 OCR 流程 |
| Tesseract 与 `chi_sim`、`eng` 语言包 | 用户主动启用的中文／英文扫描件 OCR |

## 网络与数据安全

FileSage 没有登录、权限分用户或 HTTPS 终止。默认仅监听回环地址。局域网使用时只绑定可信私有网卡、设置 `FILESAGE_ALLOWED_HOSTS` 并配置主机防火墙；不要暴露到公网或不可信网络。当前服务不支持通过 HTTP 反向代理访问 HTTPS 页面。

模型密钥只通过服务进程环境注入。目录列表不把文件正文写入任务或审计记录；正文只在用户确认相应 Agent 分析任务和数据范围后发送给模型。模型接入与隐私边界见 [Agent API 说明](agent-conversation-api.md)。
