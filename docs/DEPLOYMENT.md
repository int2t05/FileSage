# Linux 部署

FileSage 直接读写服务器原生文件系统。首次部署先使用独立测试目录验证路径、挂载和账户权限，再指向个人数据。

## 安装 Release 包

Release 包适用于 Linux amd64，不需要在服务器安装 Go 或 Node.js。从 [GitHub Releases](https://github.com/int2t05/FileSage/releases) 下载 tar 包和校验文件。若服务器无法直连 GitHub，可在可联网设备下载后传至服务器：

```sh
scp filesage_vX.Y.Z_linux_amd64.tar.gz filesage_vX.Y.Z_linux_amd64.tar.gz.sha256 user@server:~/
```

在服务器进入两份文件所在目录，先校验再解压：

```sh
set -eu
sha256sum -c filesage_vX.Y.Z_linux_amd64.tar.gz.sha256
tar -xzf filesage_vX.Y.Z_linux_amd64.tar.gz
cd filesage_vX.Y.Z_linux_amd64
```

包内含 `filesage`、预构建页面 `web/`、systemd 模板、环境示例、部署说明和许可证文件。推送指向 `main` 历史的已审阅 `vMAJOR.MINOR.PATCH` 标签会触发发布工作流：只读验证任务完成密钥扫描、Go／Web 检查、systemd 校验和 Linux 包冒烟；独立发布任务校验安装包 SHA-256 与标签目标后创建 Release。发布写权限不提供给构建、测试或依赖安装步骤。当前仓库尚无稳定版 Release。

## 从源码构建

需要 Go 1.27.3 或更新版本、Node.js 24 和 npm：

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
| `FILESAGE_UPLOAD_MAX_BYTES` | 否 | 单文件上传上限，默认 20 GiB；设为 `0` 表示不限制。超过上限时服务在接收前返回 HTTP 413。 |
| `FILESAGE_UPLOAD_STAGING_MAX_BYTES` | 否 | 未完成及等待发布的上传声明总量上限，默认 20 GiB；必须大于 `0`。超限的新上传返回 HTTP 413。 |
| `FILESAGE_WEB_DIR` | 否 | 页面目录；源码构建时通常为 `web/dist`，systemd 示例使用 `/usr/local/lib/filesage/web`。设置后由 Go 同源托管页面和 API。 |
| `FILESAGE_LISTEN_ADDR` | 否 | 监听地址，默认 `127.0.0.1:8080`。只允许回环、私有或链路本地 IP。 |
| `FILESAGE_ALLOWED_HOSTS` | 否 | 逗号分隔的额外主机名／IP，不带端口。 |
| `FILESAGE_AGENT_BASE_URL` | 否 | OpenAI-compatible Chat Completions 根地址；远程地址须用 HTTPS。 |
| `FILESAGE_AGENT_MODEL` | 否 | Agent 模型名；不配置时基础文件管理仍可使用。 |
| `FILESAGE_AGENT_PROVIDER` | 否 | 展示用服务标识，默认 `openai-compatible`。 |
| `FILESAGE_AGENT_API_KEY` | 否 | 进程环境变量中的 Bearer 密钥；不写入数据库、日志或 API 响应。 |

暂存容量按 TUS 上传声明的文件大小预留，服务重启后从暂存状态恢复；取消或成功发布后释放。最多保留 128 个未完成或等待发布的上传。系统不会自动清理仍可续传的上传；发现无法核算的暂存状态时，会保留已有数据并拒绝新建上传。

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
sudo find /usr/local/lib/filesage/web -type d -exec chmod 0755 {} +
sudo find /usr/local/lib/filesage/web -type f -exec chmod 0644 {} +
sudo install -d -o root -g root -m 0755 /etc/filesage
sudo install -o root -g root -m 0600 deploy/filesage.env.example /etc/filesage/filesage.env
sudoedit /etc/filesage/filesage.env
```

如需把模型密钥与普通配置分开，可使用独立的 root 私有环境文件：

```sh
sudo touch /etc/filesage/filesage-server.env
sudo chown root:root /etc/filesage/filesage-server.env
sudo chmod 0600 /etc/filesage/filesage-server.env
sudoedit /etc/filesage/filesage-server.env
```

在服务 drop-in 的 `[Service]` 中添加：

```ini
EnvironmentFile=-/etc/filesage/filesage-server.env
```

不要将真实密钥写入仓库或示例文件。

从源码目录部署时，将复制命令中的 `web/` 改为 `web/dist/`。

只对专为 FileSage 新建的目录授予所有权：

```sh
sudo install -d -o filesage -g filesage -m 0750 /srv/filesage-files
```

已有或共享的数据卷应只为选定目录配置 ACL 或专用组权限。不要对整块卷递归 `chown`，也不要使用 `chmod -R 777`。服务账户需在每个管理卷上拥有目录遍历、读取和所需写入权限。

unit 的 `RequiresMountsFor` 必须与实际 `FILESAGE_ROOT` 和启动时必需的独立挂载相符；systemd 不会从 `EnvironmentFile` 展开目录变量。模板启用 `ProtectSystem=strict`，并通过 `ReadWritePaths=/srv/filesage-files` 开放默认管理根目录；`StateDirectory=filesage` 自动保持可写。更改 `FILESAGE_ROOT` 时同步调整 `ReadWritePaths`，更改 `FILESAGE_DATA_DIR` 时也将数据目录加入白名单。数据库目录必须位于管理根目录之外。

模板固定 `UMask=0022`，使服务在不同主机上创建用户文件时保持一致的 POSIX 权限行为；SQLite 状态目录、上传暂存和 Agent 临时目录使用 `0700`，临时文件使用 `0600`。

模板保留 `Delegate=cpu memory pids` 和 user namespace，供 Shell 子任务隔离与资源限制使用；不要启用 `ProtectControlGroups`、`ProtectHostname`、`ProtectKernelTunables`、`ProtectKernelLogs` 或 `RestrictNamespaces`，这些设置会阻止 Bubblewrap 建立所需的私有 cgroup、UTS 或 proc 环境。服务以非特权账户运行，能力边界包含 Bubblewrap 所需的 `CAP_SYS_ADMIN`、`CAP_NET_ADMIN`，以及在 ACL／专用组授权的异主文件上设置读写租约所需的 `CAP_LEASE`。`CAP_LEASE` 不绕过文件读写权限，但可对进程能打开的文件设置租约；应用只在管理根目录内的文件操作中使用它，保留其余 systemd 文件系统限制。若 drop-in 重设 `CapabilityBoundingSet` 或 `AmbientCapabilities`，应保留这三项边界并授予 `AmbientCapabilities=CAP_LEASE`；空值会清除模板中的 ambient 能力。部署后可用 `systemctl show filesage.service -p CapabilityBoundingSet -p AmbientCapabilities` 核对能力配置。FileSage 启动 Bubblewrap 前通过 util-linux 的 `setpriv` 清除子进程 ambient capabilities，避免把文件租约能力传给沙箱；不要改用 setuid Bubblewrap。服务需要局域网监听及访问配置的模型端点，因此保留主机网络和 `AF_UNIX`、`AF_INET`、`AF_INET6`；`AF_NETLINK` 供 Bubblewrap 初始化无网络的隔离命名空间使用。

压缩包、图片和视频由独立工具解析；这些子进程通过 `setpriv` 清除 ambient capabilities，并只接收最小环境，不继承模型 API 密钥。Linux 缺少 `setpriv` 时对应操作会失败关闭。

unit 会等待 `network-online.target` 再启动。启用当前网络管理器对应的 wait-online 服务，确保固定监听地址已配置；例如 NetworkManager 使用 `NetworkManager-wait-online.service`，systemd-networkd 使用 `systemd-networkd-wait-online.service`。

若另一个卷挂载在管理根目录的子目录，为它添加 systemd drop-in；每个启动前必须就绪的挂载点各列一项：

```ini
# /etc/systemd/system/filesage.service.d/local-volumes.conf
[Unit]
RequiresMountsFor=/srv/filesage-files/volume-1
```

保存后运行 `sudo systemctl daemon-reload`，再用 `systemctl show filesage.service -p RequiresMountsFor` 确认根目录和子卷均已列出。

安装并校验 unit 后启动：

```sh
sudo install -o root -g root -m 0644 deploy/filesage.service /etc/systemd/system/filesage.service
sudo systemd-analyze verify /etc/systemd/system/filesage.service
sudo systemctl daemon-reload
sudo systemctl enable --now filesage.service
sudo systemctl status filesage.service
sudo systemctl show filesage.service -p Wants -p After -p RequiresMountsFor -p CapabilityBoundingSet -p AmbientCapabilities
```

更改 unit 后先运行 `daemon-reload`。更改环境文件后重启服务。运行日志由 systemd journal 接收，使用 `sudo journalctl -u filesage.service` 查看。每个 HTTP 响应均包含 `X-Request-ID`；日志仅记录错误与慢请求。超过 2 秒的成功 TUS 分块按 INFO 记录，其他慢请求按 WARN 记录，4xx 为 WARN、5xx 为 ERROR。日志只记录路由模板、状态和耗时，不写入路径、查询参数或正文；Agent 日志只记录会话／计划／操作 ID、动作、状态、错误类别和耗时。

## 更新版本

保持现有环境文件、管理根目录、数据目录和服务器专用 drop-in 不变。先校验发布包并解压。若版本包含数据库迁移，先按“备份与恢复”完成停服一致性备份并校验校验和；迁移失败时修复前向版本或恢复完整快照，不要单独回退旧二进制。

```sh
set -eu
sha256sum -c filesage_vX.Y.Z_linux_amd64.tar.gz.sha256
tar -xzf filesage_vX.Y.Z_linux_amd64.tar.gz

app=/usr/local/lib/filesage
release="$PWD/filesage_vX.Y.Z_linux_amd64"
rollback="$app/rollback-before-vX.Y.Z"
stage="$app/.release-vX.Y.Z"
test -x "$release/filesage"
test -s "$release/web/index.html"
test ! -e "$rollback"

sudo install -d -o root -g root -m 0700 "$rollback"
sudo install -d -o root -g root -m 0755 "$stage/web"
sudo install -o root -g root -m 0755 "$release/filesage" "$stage/filesage"
sudo cp -R "$release/web/." "$stage/web/"
sudo find "$stage/web" -type d -exec chmod 0755 {} +
sudo find "$stage/web" -type f -exec chmod 0644 {} +
sudo chown -R root:root "$stage"

sudo systemctl stop filesage.service
sudo mv "$app/filesage" "$rollback/filesage"
sudo mv "$app/web" "$rollback/web"
sudo mv "$stage/filesage" "$app/filesage"
sudo mv "$stage/web" "$app/web"
sudo systemctl start filesage.service
curl --fail --silent --show-error http://127.0.0.1:8080/api/v1/health
sudo rmdir "$stage"
```

若 `FILESAGE_LISTEN_ADDR` 绑定了其他地址，健康检查应替换为对应地址。数据库迁移在服务启动时执行。包含 `delete` 恢复记录的数据库不能直接交给不兼容的旧版启动；若日志报告待核查路径，先按源路径与隔离路径恢复，或修复当前版本，不要直接回退旧二进制。无待恢复操作且无数据库迁移时，可恢复程序副本：

```sh
set -eu
sudo systemctl stop filesage.service
sudo install -d -o root -g root -m 0700 "$stage/failed"
sudo mv "$app/filesage" "$stage/failed/filesage"
sudo mv "$app/web" "$stage/failed/web"
sudo mv "$rollback/filesage" "$app/filesage"
sudo mv "$rollback/web" "$app/web"
sudo systemctl start filesage.service
curl --fail --silent --show-error http://127.0.0.1:8080/api/v1/health
```

若 schema 已迁移且旧版本不兼容，应修复前向版本或恢复完整快照。检查 `sudo journalctl -u filesage.service -n 100 --no-pager`；确认服务稳定后保留一份立即可用的程序回退副本，再清理更早的重复程序备份。完整数据恢复快照独立保留。

若发布包的 systemd 模板有变化，先审阅 `systemctl cat filesage.service`，将本机主 unit 另行备份后再更新；保留服务器专用 drop-in 和环境文件。运行 `systemd-analyze verify` 与 `systemctl daemon-reload` 后再重启服务。

从源码构建时，将上面的 `release` 路径指向构建输出，并将 `web/` 指向 `web/dist/`。

## 备份与恢复

`FILESAGE_ROOT` 保存原始文件和 `.filesage-trash`；`FILESAGE_DATA_DIR` 保存 SQLite、上传暂存及 Agent 暂存。可恢复备份必须在同一停服窗口包含两者，并遍历管理根目录下的所有已挂载卷。备份目标应是与数据源物理独立或异地的加密存储；暂停其他写入进程，并核对 `findmnt` 挂载及 `lsblk` 设备归属。备份包含个人文件和任务记录，应限制访问；模型密钥与挂载配置另行安全保存，不要放入公开仓库。

以下示例使用 GNU tar。按实际路径修改变量，并在循环中列出管理根目录下的每个预期子卷挂载点；根目录本身是普通目录时不要把它列入。脚本只核验清单中的挂载，不会自动发现遗漏的子卷；备份前须对照 `/etc/fstab` 和 `findmnt` 逐项检查。清单中任一挂载缺失时脚本停止。

```sh
set -eu
root=/srv/filesage-files
data=/var/lib/filesage
backup_mount=/mnt/backup
backup="$backup_mount/filesage/$(date -u +%Y%m%dT%H%M%SZ)"

sudo mountpoint -q "$backup_mount" || { echo "backup volume is not mounted" >&2; exit 1; }
sudo systemctl is-active --quiet filesage.service || { echo "filesage.service is not active" >&2; exit 1; }
sudo findmnt -T "$root"
for volume in "$root/volume-1"; do # 为每个必需的子卷挂载点增加一项
  sudo mountpoint -q "$volume" || { echo "required volume is not mounted: $volume" >&2; exit 1; }
  sudo findmnt -T "$volume"
done
sudo findmnt -T "$backup_mount"
sudo install -d -o root -g root -m 0700 "$backup_mount/filesage"
sudo mkdir -m 0700 "$backup"

trap 'sudo systemctl start filesage.service' 0
sudo systemctl stop filesage.service
sudo tar --acls --xattrs --numeric-owner -cpf "$backup/managed-root.tar" -C "$root" .
sudo tar --acls --xattrs --numeric-owner -cpf "$backup/service-data.tar" -C "$data" .
sudo bash -c 'cd "$1" && sha256sum managed-root.tar service-data.tar > SHA256SUMS' _ "$backup"
sudo bash -c 'cd "$1" && sha256sum -c SHA256SUMS' _ "$backup"
sudo systemctl start filesage.service
trap - 0
```

恢复前先校验校验和，确认所有目标卷在线，并将目标准备为空目录；恢复循环须列出与备份时相同的完整子卷清单。tar 解包不会删除备份中不存在的旧文件。示例在完整性检查通过后才启动服务；检查失败时保持服务停止并排查。真实恢复前先在隔离路径演练，并保留当前状态副本；不要把归档直接合并到非空生产目录。

```sh
set -eu
root=/srv/filesage-files
data=/var/lib/filesage
backup_mount=/mnt/backup
backup_name=REPLACE_WITH_BACKUP_NAME # 替换为实际备份目录名
backup="$backup_mount/filesage/$backup_name"

sudo mountpoint -q "$backup_mount" || { echo "backup volume is not mounted" >&2; exit 1; }
for volume in "$root/volume-1"; do # 与备份时列出相同的必需挂载点
  sudo mountpoint -q "$volume" || { echo "required volume is not mounted: $volume" >&2; exit 1; }
  sudo findmnt -T "$volume"
done
sudo findmnt -T "$root"
sudo bash -c 'cd "$1" && sha256sum -c SHA256SUMS' _ "$backup"
sudo systemctl stop filesage.service
# 先确认所有挂载在线，且 root、data 与每个子卷目标均为空。
sudo tar --acls --xattrs --numeric-owner -xpf "$backup/managed-root.tar" -C "$root"
sudo tar --acls --xattrs --numeric-owner -xpf "$backup/service-data.tar" -C "$data"
integrity="$(sudo sqlite3 -readonly "$data/filesage.sqlite" 'PRAGMA integrity_check;')"
[ "$integrity" = "ok" ] || { echo "database integrity check failed" >&2; exit 1; }
foreign_key_violations="$(sudo sqlite3 -readonly "$data/filesage.sqlite" 'PRAGMA foreign_key_check;')"
[ -z "$foreign_key_violations" ] || { echo "database foreign-key check failed" >&2; exit 1; }
sudo systemctl start filesage.service
curl --fail --silent --show-error http://127.0.0.1:8080/api/v1/health # 按 FILESAGE_LISTEN_ADDR 替换
```

`sqlite3` 命令行仅用于恢复验收，不是运行依赖；未安装时先按系统发行版提供的受信来源安装。

## 卸载与清理

先从环境文件确认 `FILESAGE_ROOT` 和 `FILESAGE_DATA_DIR`。下面移除服务、默认环境文件和程序文件，保留原始文件、SQLite、上传暂存和任务记录：

```sh
sudo systemctl disable --now filesage.service
sudo rm -f /etc/systemd/system/filesage.service
sudo systemctl daemon-reload
sudo rm -f /etc/filesage/filesage.env
sudo rm -rf /usr/local/lib/filesage
```

若创建过 systemd drop-in，先列出并逐项审阅，只删除 FileSage 专用配置。默认保留 `FILESAGE_ROOT`、`FILESAGE_DATA_DIR`、挂载卷和 `filesage` 系统账户，便于以后恢复部署。永久清除数据前，应先核对完整备份及所有挂载点，并逐项确认目标路径；不要递归删除挂载根目录。

卸载前可用 `systemctl show filesage.service -p EnvironmentFiles -p DropInPaths` 核对实际加载项。上面的命令只删除模板默认环境文件；若使用了额外密钥文件或服务器专用 drop-in，应按输出逐项移除其 FileSage 专用文件，不要递归清除可能存放其他服务配置的目录。

## 可选工具

能力缺失时对应功能会明确不可用。发行版安装包名称可能不同。

| 工具 | 功能 |
| --- | --- |
| `sqlite3` | 离线备份恢复完整性检查；不是运行依赖。 |
| `ffmpeg`、util-linux（`setpriv`） | 视频海报 |
| `heif-dec` 或 `heif-convert`、`dcraw_emu`、`prlimit`、`setpriv` | HEIF／HEIC 与 RAW 图片预览 |
| `7z`、`7zz` 或 `7za`、`setpriv` | 7z／RAR 浏览，7z 归档创建 |
| LibreOffice、Bubblewrap（`bwrap`）、`prlimit`、util-linux（`setpriv`） | PPTX 预览；Bubblewrap 和 systemd cgroup v2 委托也用于 Agent Shell |
| Poppler `pdftotext`、`pdftoppm`、`pdfinfo`，Bubblewrap、`prlimit`、`setpriv` | Office／PDF 正文提取与 OCR 流程 |
| Tesseract 与 `chi_sim`、`eng` 语言包 | 用户主动启用的中文／英文扫描件 OCR |

## 网络与数据安全

FileSage 没有登录、权限分用户或 HTTPS 终止。任何能访问服务的网络客户端都可操作管理文件、切换 Agent 审批模式、创建并批准任务，也可消耗已配置模型的调用额度。写请求会拒绝跨站来源，但无 `Origin` 的客户端仍可调用；同源校验不提供身份认证。默认仅监听回环地址；局域网使用时只绑定可信私有网卡并设置 `FILESAGE_ALLOWED_HOSTS`，不要暴露到公网或不可信网络。当前服务不支持通过 HTTP 反向代理访问 HTTPS 页面。

模型密钥只通过服务进程环境注入。Agent 对话消息会在每轮交流时发送给配置模型；文件正文不会在澄清阶段读取，只有用户确认相应分析卡后才按卡片列明范围读取并发送。正文不写入任务或审计记录。模型接入与隐私边界见 [Agent API 说明](agent-conversation-api.md)。
