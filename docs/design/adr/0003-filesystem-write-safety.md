# ADR-0003：根目录受限访问与协调提交

## 状态

**已决定。** 当前实现的验证证据和未覆盖边界统一记录在[验收记录](../../ACCEPTANCE.md)。

## 背景

管理根目录可能包含多挂载卷、软链接和硬链接；应用、Shell、上传、阅读器和外部工具会并发访问。需要拒绝越界，避免半写文件，发现审批后文件变化，并支持跨文件系统复制移动。用户明确不保留旧文件内容。

## 决策

所有应用写入口集中到文件操作服务。使用 Go 1.27.3+ 的 `os.Root`，所有 API 路径为根目录相对路径；允许根目录内挂载点，拒绝越界软链接。写入先构造新内容并校验，再从同一文件系统暂存区原子替换；移动跨文件系统时分阶段复制、校验、目标发布、源删除并持久记录进度。所有批准包含路径、方案和源状态指纹，执行时重验。

Linux 替换已有文件先对当前文件申请读租约，阻止或检测外部写入者，再使用 `renameat2(RENAME_EXCHANGE)` 交换候选与目标并复核被交换出的源身份和内容摘要；冲突时尝试原子交换回去。无法申请租约或文件系统不支持租约时拒绝覆盖。不支持交换的文件系统同样拒绝覆盖，回滚失败会保留临时路径并报告给用户。成功后移除旧目录项，不保留历史副本。最终租约检查与移除旧目录项仍非原子操作，不能保证与非协作外部访问形成全局事务。

Linux 永久删除在移除前创建同卷 `0700` 隔离目录，并先写入持久恢复记录。服务复核源目录项后以 `RENAME_NOREPLACE` 将其移入隔离目录，再核对隔离项身份、内容状态和目录是否为空；文件系统不支持该操作时拒绝删除。校验失败时仅在原路径空闲时恢复；否则保留记录和隔离项供人工核查。删除提交后同步目录并清理隔离目录与记录。重启恢复只处理有日志的操作，不覆盖已有路径；用户确认的删除一旦提交仍不可恢复，也不保留旧内容历史。

对多链接 inode（`nlink > 1`），所有内容编辑、覆盖和 Shell 写回都拒绝，包括手动编辑。按路径重命名和删除链接项仍可用；这些操作只影响对应目录项。内部软链接可用于查看和浏览；涉及写入时解析为明确目标并按 `(device, inode)` 协调，删除软链接只删除链接本身。若目标无法可靠解析至根目录内的直接路径，拒绝写入并提示用户从目标路径执行。

## 备选方案

| 方案 | 优点 | 代价／拒绝理由 |
| --- | --- | --- |
| 直接按路径检查前缀后操作 | 实现简单 | 软链接替换和 TOCTOU 可穿越边界，拒绝 |
| 原目录直接原地写入 | 保留硬链接关系 | 读者可能看见部分内容；外部硬链接同样被改写；失败恢复困难 |
| 逐路径原子替换 | 单路径读者看见旧或新文件 | 多硬链接目录项无法作为一个全局原子事务切换 |
| 暂存后统一协调提交 | 易校验、易保持文件完整并绑定审批状态 | 需要暂存空间；跨盘复制较慢；多路径及目录树提交仍需部分完成状态 |

## 影响

根目录约束不等于 Linux 权限提升；服务以非 root 用户运行，只能修改其有权访问的文件，`CAP_LEASE` 不替代读写权限，`chown` 按实际权限失败。内部软链接须解析至根目录；硬链接不能靠 `realpath` 识别。隔离目录阻止不同 UID 的普通进程通过原路径替换待删项；服务账户同 UID 的进程仍在信任边界内。进程内协调也不限制外部程序直接修改其他写入目标。替换和已提交删除后不恢复旧内容。旧版不识别未完成的 `delete` 恢复记录；回退前须确认无待核查项，否则应前向修复或人工恢复。

## 证据

- `internal/agent/move_file_linux_test.go` 的 `TestRenameRejectsExternalProcessChangeBetweenDigestAndCommit` 在源身份检查后、原子改名前启动独立测试进程改写源文件；冲突被检测、来源路径恢复。Linux 用例重复 20 次、Agent 全包 race 检查通过。该结果只覆盖同卷 Agent 改名的这一提交窗口。
- `internal/fsroot/write_linux_test.go` 的 `TestWriteAtomicIfUnchangedRejectsExternalOpenWriter` 由独立进程持有原文件写句柄，再尝试替换；操作返回冲突，原路径仍指向原 inode，进程外更新可从原路径读取。该用例在 Linux 重复 20 次通过。
- 同文件的 `TestWriteAtomicFromRejectsExternalOpenWriter` 在修复前复现 Shell 流式写回绕过租约并覆盖原路径；修复后改用与普通编辑相同的原子交换提交。两个入口的外部写句柄用例在隔离 Linux 环境各重复 20 次通过，原 inode 与进程外更新保留且无暂存残留；全量 Go 测试、针对性 race 和 `go vet` 通过。Agent Shell 修改已有文件的闭环在独立 systemd 测试单元中实际运行并通过。
- `internal/fsroot/write_linux_test.go` 与 `move_tree_linux_test.go` 使用独立进程持有源文件写句柄，并验证快照后的已完成改写会在删除前触发冲突；目录移动先预检整棵树，避免预检发现冲突前清掉部分来源。七组关键移动用例在 Linux 重复 20 次通过，完整 Go 测试、`fsroot` 与 Agent race、`go vet` 通过。Linux 写租约需要文件系统支持。
- `internal/trash/manager_test.go` 的 `TestPermanentRemovePreservesReplacementAfterAuditWrite` 在永久删除开始审计期间替换路径，确认操作返回冲突并保留两项文件；`internal/fsroot/write_linux_test.go` 覆盖过期条目、活动写句柄、普通文件、空目录和软链接删除。关键删除用例在 Linux 重复 20 次通过，`fsroot` 与 trash 全包、race、`go vet` 通过。
- `internal/fsroot/remove_entry_linux_test.go` 覆盖提交边界替换、中断恢复、源路径冲突、异常隔离项身份和已提交后不恢复。Linux Go 1.27.3 全量 `go test ./...`、`go vet ./...`、`go build ./...` 及七组关键恢复／竞态用例 `-race -count=20` 通过；主线 CI 及服务器部署后健康检查通过。
- `internal/httpapi/write_test.go` 与 `batch_rename_test.go` 覆盖审计写入期间替换源路径，以及改名返回前目标身份变化；前者拒绝执行并结束失败审计，后者记为 `needs_review`，停止后续批次且不报告成功。改名后的身份核对不能消除系统调用窗口，也不自动恢复错误目标。Linux 全量 Go 测试、`httpapi`／`fsroot`／trash race 与全量 `go vet` 通过。
- `internal/agent/recovery_process_linux_test.go` 在 20,000 文件复制已写入部分私有暂存后终止 API 子进程；重启清除暂存，计划要求明确重试，目标未发布且未自动重放。主线全量测试包含此用例；测试服务器专项 race `-race -count=3` 通过。
- 隔离内核探针在服务器 `/tmp` ext4 上验证了直接从原路径 `unlink` 时租约检查与删除之间的竞态；该证据促成本次改用先隔离、再核验身份的删除流程。探针使用临时目录，未访问生产服务或真实用户文件；它不覆盖服务账户同 UID 进程对隔离目录的访问。
- [Linux `F_SETLEASE`／`F_GETLEASE`](https://man7.org/linux/man-pages/man2/F_SETLEASE.2const.html) 说明文件租约会在冲突访问时被打破并可能阻塞调用方；该机制不使目录项删除与租约状态检查成为一个原子事务。
- [Linux `unlinkat(2)`](https://man7.org/linux/man-pages/man2/unlinkat.2.html) 按目录句柄和路径名删除目录项，唯一支持的标志为 `AT_REMOVEDIR`；接口不提供按已打开文件身份原子删除的操作。
- [Linux `fsync(2)`](https://man7.org/linux/man-pages/man2/fsync.2.html)：文件同步不保证目录项已落盘；目录也需显式同步。
- 2026-10-22：服务器上以 `filesage` 身份对另一 UID 所有、但可读的临时文件申请租约返回 `EPERM`；同一操作在仅授予 `AmbientCapabilities=CAP_LEASE` 的临时 systemd 单元中通过。测试文件位于 `/tmp`，已删除；持久服务未修改。
- [Go traversal-resistant file API](https://go.dev/blog/osroot) 与[GO-2026-4970 漏洞公告](https://pkg.go.dev/vuln/GO-2026-4970)（访问 2026-10-15）：说明目录句柄路径 API 的能力及版本修复要求。
- [Linux `openat2`](https://man7.org/linux/man-pages/man2/openat2.2.html)（访问 2026-10-15）：`RESOLVE_IN_ROOT` 将绝对路径／软链接按指定根解析；不能禁用所有挂载点，因为管理根目录允许多卷。
- [Linux rename](https://man7.org/linux/man-pages/man2/rename.2.html)、[软链接](https://man7.org/linux/man-pages/man7/symlink.7.html)、[chmod](https://man7.org/linux/man-pages/man2/chmod.2.html)、[chown](https://man7.org/linux/man-pages/man2/chown.2.html)：官方手册用于提交和权限实现。
- [Google safeopen](https://github.com/google/safeopen/blob/43626d6f468560fc611ad54f1f17f3a455dbe92d/README.md)：Go 路径约束备选；须对照当前 Go API 与漏洞修复后再决定是否引依赖。

## 决策记录

2026-10-15：采用 `os.Root` 路径约束、同文件系统暂存后原子替换、跨盘分阶段移动和执行前状态复核；服务以非 root 账户运行。为避免硬链接别名下的内容覆盖，`nlink > 1` 文件拒绝编辑、覆盖和 Shell 写回。外部进程竞态不受应用内锁约束。

2026-10-23：永久删除改为同卷隔离目录、持久恢复记录和提交后清理；`4094793` 已合入、通过 CI 并部署。新增 Agent Linux API／SQLite 进程中断恢复测试并合入 `cd2ec48`；主线 CI、测试服务器全量检查及专项 race ×3 通过。其他动作中断、隔离项的同 UID 访问及真实介质故障仍待验收。

2026-10-24：7-Zip、图片解码器和 ffmpeg 统一经 `internal/safecmd` 启动，避免外部解析器继承模型凭据和 `CAP_LEASE`。服务账户与 systemd 能力配置不变；Linux 包测试及 Race 通过。
