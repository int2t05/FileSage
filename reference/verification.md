# 调研资料核验

核验日期：2026-10-24（Asia/Shanghai）。资料采集日期见来源清单。

结果：通过。

- 仓库：110，README：110。
- 本地固定提交文档：1308，原文字节：9,940,317。
- 上游原文快照：110/110 个项目。
- 重点仓库目录树：70。
- 官方页面：26，本地 HTML／正文文件：52。
- 文档查询快照：3，JSON 结构与内容检查通过。
- 自写文档本地链接检查：76。
- 对存在的本地快照核对文件、字节长度和 SHA-256，并检查固定提交来源。

## 分类数量

| 类别 | 仓库数 |
| --- | --- |
| ai-organization | 12 |
| document-management | 6 |
| file-management | 18 |
| frontend-design | 8 |
| implementation-reference | 10 |
| personal-cloud | 6 |
| photo-management | 11 |
| technical-reference | 39 |

## 证据限制

竞品未部署运行；文档能力声明不等于实测通过。上游文件原文、注释和版本描述按原样保留，自写代码注释采用中文。上游文档内未采集外链不参与完整性检查。

公开仓库不分发上游原文；缺少本地快照时会跳过原文哈希检查。

未采到独立许可证文件的仓库：SeyiDan/smart-file-organizer、ShichengRao/smart-file-organizer、kalcaddle/kodbox、kalcaddle/KodExplorer、lrsjng/h5ai、sigoden/dufs、qarmin/czkawka、quantumx-apps/filebrowserDocs、futurepress/epub.js。缺少独立文件不代表没有许可证，复用实现前需检查上游。

候选文件不存在或读取失败的记录保留在 sources.json 的 unavailable 中；这些记录不等于核心采集失败。

## 问题

无文件完整性或自写文档链接问题。
