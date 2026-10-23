# ADR-0005：混合式只读阅读器

## 状态

**方向已决定；多格式基础实现及 Office／归档 Linux 集成验收通过，RAW／HEIF 服务端点和复杂真实样本矩阵仍待验证。**

## 背景

首期需在浏览器内阅读图像、视频、PDF、Markdown、代码、DOCX、XLSX/CSV/TSV、PPTX、EPUB 和压缩包条目。DOCX/XLSX/PPTX 阅读只读；需展示书签、工作表、章节、幻灯片或包内导航。不得执行文件脚本／宏／外链，也不要求前端 Office 编辑或全格式转码。

## 决策方向

对各格式组合专用浏览器阅读器和按需派生预览。PDF.js 负责 PDF；CodeMirror 6 与经过消毒的 Markdown 渲染负责文本；epub.js 负责 EPUB；SheetJS/ExcelJS 负责工作表数据呈现；DOCX/PPTX 先评估 LibreOffice 隔离转换为只读 PDF，再单独解析标题或幻灯片导航。图像／缩略图、元数据、OCR、归档分别调用对应库或受限工具。派生文件使用可失效缓存，不作为原件备份。

## 备选方案

| 方案 | 优点 | 代价／取舍 |
| --- | --- | --- |
| 纯浏览器解析 | 不需要转换服务器；工作表和章节结构可保留 | Office 排版保真和中文字体表现有限；多格式解析增加前端包和攻击面 |
| 服务端全转 PDF | DOCX/PPTX 分页阅读较自然，可复用 PDF.js | 不能代替 Excel 工作表和归档导航；转换慢、字体差异及子进程安全成本 |
| ONLYOFFICE DocumentServer / Collabora | 格式覆盖广，完整在线套件 | 独立服务资源与更新成本，带入超出只读需求的完整平台和许可义务 |
| 按格式混合（建议） | 用结构化读取满足导航，用转换补充页面呈现 | 多阅读路径需真实样本逐类维护；任何一条失败仍须明确支持矩阵 |

## 影响

须维护真实样本集，包括复杂 DOCX 图文表格、含公式缓存值的 XLSX、带中文字体与图表的 PPTX、受损／加密归档、不同布局 EPUB 和 EXIF 各异图片。转换、OCR 与媒体解码应有资源限制、错误／重试和源状态缓存键。Office 一律无宏、无外链和只读；内容解析结果视为不可信，渲染需 CSP 和 HTML 消毒。

## 证据

- [PDF.js 官方阅读 API](https://mozilla.github.io/pdf.js/getting_started/?lang=en)（访问 2026-10-15）：浏览器分页渲染入口。
- [LibreOffice 官方转换过滤器](https://help.libreoffice.org/latest/en-US/text/shared/guide/convertfilters.html)（访问 2026-10-15）：支持命令行格式转换；实际保真度仍待样本验证。
- [SheetJS 官方表格资料](https://docs.sheetjs.com/docs/api/utilities/html/) 与固定提交的 [ExcelJS](https://github.com/exceljs/exceljs/blob/5bed18b45e824f409b08456b59b87430ded023ab/README.md)：工作表读取／展示参考，不代表公式重算或完整 Excel 排版。
- [docxjs](https://github.com/VolodymyrBaydalka/docxjs/blob/c533f383069008d4a1e05c6171ef8777fd078385/README.md)、[PPTXjs](https://github.com/meshesha/PPTXjs/blob/1a9260b2062f89ba822aeb54da792236780af8e7/README.md)、[EPUB.js](https://github.com/futurepress/epub.js/blob/eee359d0790002115a1156a9833c54f4bcd44c1d/README.md)、[libarchive](https://github.com/libarchive/libarchive/blob/76d41c18cf53c888afb2c9477fd595a5d3984192/README.md)：格式专项候选，均未按 FileSage 样本运行。
- [ONLYOFFICE DocumentServer 许可证](https://github.com/ONLYOFFICE/DocumentServer/blob/master/LICENSE)（访问 2026-10-15）：仓库公布 AGPLv3 与附加条款；集成和分发前需法律／许可复核。

## 决策记录

2026-10-15：采用浏览器格式专用阅读器和受限 LibreOffice 派生预览，不部署完整 Office 服务。发布依赖需通过许可证、维护状态、安全更新和真实样本核验；复杂文件的渲染保真度仍未验收。
