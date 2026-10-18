# ADR-0006：Shell 在暂存沙箱运行并审批差异

## 状态

**已决定；临时委托 systemd 下的 Linux 沙箱原语验收通过，Agent Shell 已接入审批与写回；目标 FileSage 服务部署和浏览器闭环仍待验证。**

## 背景

PRD 允许 Agent 使用 Shell，但审批前必须列出命令、工作目录和具体文件影响；审批后不能绕过统一并发写入控制、根目录限制和逐项审批。动态 Shell 命令的副作用无法由静态文本可靠预测。

## 建议决策

不在原目录直接执行待审批 Shell。将命令、参数、工作目录、输入范围放入 Bubblewrap 隔离环境：根目录只读绑定，独立暂存目录可写，隔离 PID／用户／网络命名空间，不挂载宿主套接字、密钥或设备；限制时间、CPU、内存、进程数、输出和临时空间。命令结束后从暂存区计算差异，列出新增、修改、删除路径及内容／权限预览。用户逐项批准后，统一提交器核对源文件状态并应用。不能完整隔离或列出差异时拒绝执行／提交。

## 备选方案

| 方案 | 优点 | 代价／取舍 |
| --- | --- | --- |
| 直接在原目录运行 Shell | 最兼容；命令结果可立即使用 | 无法可靠提前列出具体影响；命令能绕过锁和批准，拒绝 |
| 仅用容器／Bubblewrap 直接写原目录 | 降低宿主边界风险 | 沙箱不提供应用审批、差异审查和文件提交协调，不能独自满足 PRD |
| 只允许结构化文件操作，不允许 Shell | 最容易保证批准清单和幂等 | 不满足已确认 Shell 能力，故不推荐 |
| 沙箱只读输入 + 暂存差异（建议） | 审批看到实际产物；原件不受预览／分析阶段写入影响 | 暂存复制开销；某些命令依赖系统服务或外网时不兼容；最终多文件提交仍有部分完成状态 |

## 影响

命令不能依赖网络、宿主 socket、GUI、后台服务或写暂存区之外的位置；若输入数据量大，需评估文件复制／挂载和磁盘占用。Bubblewrap 依赖主机 namespace 配置和内核能力，不可用时停用 Shell，不能无声降级。提交器逐文件写回前重新审查 inode／哈希；旧内容按 PRD 不保存，因此中断或用户误批不可通过系统恢复旧文件。此方案仅限制操作路径，不替代无账号部署的网络隔离，也不证明命令逻辑正确。

## 证据

- [Bubblewrap 上游](https://github.com/containers/bubblewrap/blob/1fb0aa26d3eb66254f7742e704add7f82f41a582/README.md)：低层 Linux namespace 和 mount 配置工具；其边界由调用者配置。
- [Bubblewrap 参数与示例](https://github.com/containers/bubblewrap/blob/main/bwrap.xml)、[Shell 示例](https://github.com/containers/bubblewrap/blob/main/demos/bubblewrap-shell.sh)（访问 2026-10-15）：展示文件系统绑定、网络与 PID namespace 等参数；不能直接当作完整产品策略。
- [Linux Landlock 官方手册](https://man7.org/linux/man-pages/man7/landlock.7.html)、[NsJail](https://github.com/google/nsjail/blob/4ff54a6e0d5b65a0e1633d6e2fd1425ba6c882ce/README.md)：可用于沙箱备选评估；尚未做目标主机兼容性测试。

## 决策记录

2026-10-15：用户授权助手决定 Proposed ADR。选定 Bubblewrap 只读输入、私有暂存目录写出、实际差异逐项审批、统一提交器应用的两阶段流程。目标主机无法建立并验证完整隔离、超时／资源限制或差异清单时，Shell 功能保持关闭；不得直接运行原目录命令作为降级方案。
