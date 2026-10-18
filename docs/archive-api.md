# 压缩包 API

此接口提供压缩包目录浏览、包内文件预览、解压和归档创建。创建目前仅支持 Linux；支持 ZIP（存储模式）、7z、TAR（不压缩）和 TAR.GZ（gzip）。7z 创建依赖服务器安装 7-Zip，并需要临时空间暂存所选原件和生成结果。

浏览、预览和解压支持 ZIP、7z、RAR、TAR、TAR.GZ 和 TGZ。7z 与 RAR 依赖服务端安装 7-Zip 兼容命令；当前服务器尚无真实 RAR 样本验收记录。浏览、预览和解压合计最多并发 2 项；归档创建与 ZIP 下载合计最多并发 1 项。外部命令尚未设置操作系统级 CPU／内存限额。

## 浏览目录

GET /api/v1/files/archive/entries?path=<相对路径>&version=<文件版本>&directory=<包内目录>

path、version、directory 必填；directory 传空值表示压缩包根目录。返回当前目录条目，含名称、包内路径、类型、大小和修改时间。归档文件限 20 GiB、最多 50,000 个条目；ZIP 中央目录限 64 MiB。

## 预览文件

GET /api/v1/files/archive/content?path=<相对路径>&version=<文件版本>&entry=<包内文件路径>

成功时直接返回文件字节。响应不缓存，并回传版本号；预览内容上限为 32 MiB。浏览器只对已有阅读器支持的格式提供预览。

## 解压

POST /api/v1/files/archive/extract

请求体字段：path 为归档相对路径，version 为当前文件版本，destination 为管理根目录内现有目标目录，entries 为包内路径列表。

entries 为空表示解压全部；非空时只解压所选包内路径。目标必须是管理根目录内已存在的真实目录。已存在的名称会自动编号，不覆盖原文件。完整结果返回 201，部分完成返回 207 和逐项错误；调用者应刷新目标目录。

所有读取和解压都绑定当前归档版本；文件变化时返回冲突。解压要求同源请求，拒绝越界路径、软链接和特殊文件；单项展开限 2 GiB、总展开限 20 GiB、压缩比限 1,000，ZIP 中央目录限 64 MiB。

## 创建 ZIP、7z、TAR 与 TAR.GZ

POST /api/v1/files/archive/create

请求体字段：`items` 为 1 到 256 个 `{path, version}` 项；`destination` 为现有目标目录，空字符串表示管理根目录；根目录的 `destinationVersion` 必须为空，其他目录必须提交当前版本；`name` 的扩展名须与 format 匹配；format 支持 zip、7z、tar、tar.gz，省略时默认 zip，以兼容旧请求。

```json
{
  "items": [{"path": "文档/合同", "version": "当前版本令牌"}],
  "destination": "归档/2026",
  "destinationVersion": "当前目录版本令牌",
  "name": "合同.zip"
}
```

服务端先生成完整临时归档，再复核来源并以原子无覆盖方式发布。7z 通过安全 TAR 流将所选内容暂存到权限受限的临时目录，再由 7-Zip 打包；临时目录会在请求完成后清理。目标名称已存在时保留完整扩展名并自动编号，例如 `合同 (1).zip`；响应 `201` 返回最终 `path` 和 `name`。来源或目标版本过期时返回 `409`；来源包含软链接／特殊文件、资源超限或磁盘空间不足时返回对应错误，且不会留下未完成的归档。总计最多 50,000 个条目、20 GiB 源内容；管理根目录外的链接不跟随，目标必须是管理根目录内的真实目录。
