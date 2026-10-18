import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { describe, it } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/features/agent/AgentPage.tsx', import.meta.url), 'utf8');
const styles = await readFile(new URL('../src/features/agent/AgentPage.css', import.meta.url), 'utf8');
const duplicateReportSource = await readFile(new URL('../src/features/agent/DuplicateScanReport.tsx', import.meta.url), 'utf8');
const sourceFile = ts.createSourceFile('AgentPage.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const declarations = sourceFile.statements.filter((statement) => ts.isFunctionDeclaration(statement)
  && ['analysisCardPresentation', 'destinationLabel', 'formatBytes'].includes(statement.name?.text ?? ''));
assert.ok(declarations.some((statement) => ts.isFunctionDeclaration(statement) && statement.name?.text === 'analysisCardPresentation'), 'AgentPage 应提供确认卡展示规则');
const { outputText } = ts.transpileModule(`${declarations.map((statement) => statement.getText(sourceFile)).join('\n')}\nexport { analysisCardPresentation };`, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const presentationModule = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

describe('Agent 移动文件确认卡', () => {
  it('新建只按任务与路径生成候选，覆盖明确说明读取全文并不可恢复', () => {
    const create = presentationModule.analysisCardPresentation({
      goal: '生成会议纪要模板',
      action: 'create_file',
      filePath: '文档/会议纪要.md',
      targetPath: '',
      dataTypes: ['任务描述与目标路径'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    });
    assert.deepEqual(create.operations, [{
      number: 1,
      title: '新建文本文件',
      paths: [{ label: '新建路径', path: '文档/会议纪要.md' }],
      metadata: [],
    }]);
    assert.match(create.notice, /仅使用任务描述与目标路径.*不读取文件正文/);
    assert.equal(create.confirmLabel, '确认并生成候选');
    assert.equal(create.createsPlanOnly, true);

    const overwrite = presentationModule.analysisCardPresentation({
      goal: '修复文档内容',
      action: 'overwrite_file',
      filePath: '文档/原稿.txt',
      targetPath: '',
      dataTypes: ['任务描述', 'UTF-8 文件正文全文'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    });
    assert.deepEqual(overwrite.operations, [{
      number: 1,
      title: '覆盖文本文件',
      paths: [{ label: '目标文件', path: '文档/原稿.txt' }],
      metadata: [],
    }]);
    assert.match(overwrite.notice, /读取目标文件完整原文并发送给当前模型/);
    assert.match(overwrite.notice, /执行后原内容不可恢复/);
    assert.equal(overwrite.confirmLabel, '确认并读取原文');
    assert.equal(overwrite.createsPlanOnly, false);
    assert.match(source, /analysisCardView\.confirmLabel/);
  });

  it('普通删除确认卡说明只移入可恢复回收站', () => {
    const view = presentationModule.analysisCardPresentation({
      goal: '清理旧文件',
      operations: [{ action: 'trash', filePath: '文档/旧稿.md', targetPath: '' }],
      dataTypes: ['任务描述与路径，不读取正文'],
      provider: 'test-provider',
      model: 'test-model',
      destination: 'remote',
    });
    assert.equal(view.operations[0].title, '移入回收站');
    assert.deepEqual(view.operations[0].paths, [
      { label: '待回收项目', path: '文档/旧稿.md' },
      { label: '处理方式', path: '移入可恢复回收站' },
    ]);
    assert.match(view.notice, /不会修改文件/);
  });

  it('永久删除确认卡醒目标注不可恢复且确认不会立即删除', () => {
    const view = presentationModule.analysisCardPresentation({
      goal: '永久清理过期文件',
      operations: [{ action: 'permanent_delete', filePath: '文档/过期.txt', targetPath: '' }],
      dataTypes: ['任务描述与路径，不读取正文'],
      provider: 'test-provider',
      model: 'test-model',
      destination: 'remote',
    });
    assert.equal(view.operations[0].title, '永久删除（不可恢复）');
    assert.deepEqual(view.operations[0].paths, [
      { label: '待删除项目', path: '文档/过期.txt' },
      { label: '删除后', path: '无法恢复' },
    ]);
    assert.match(view.notice, /不可恢复/);
    assert.equal(view.createsPlanOnly, true);
  });

  it('仅展示源／目标路径与方案依据，并说明确认不会修改文件', () => {
    const view = presentationModule.analysisCardPresentation('move_file', '待整理/报告.pdf', '归档/报告.pdf');
    assert.deepEqual(view, {
      heading: '确认移动文件',
      paths: [
        { label: '源路径', path: '待整理/报告.pdf' },
        { label: '目标路径', path: '归档/报告.pdf' },
      ],
      metadata: [],
      basis: '任务描述、源路径与目标路径',
      notice: '确认后生成待审核方案，不会修改文件。',
      confirmLabel: '确认并生成方案',
      showModelDetails: false,
      createsPlanOnly: true,
    });
    assert.match(source, /analysisCardView\.dataTypes\?\.join\(/);
  });

  it('复制文件和文件夹只展示本机复制路径，不把正文发送给模型', () => {
    for (const [action, heading] of [['copy_file', '确认复制文件'], ['copy_directory', '确认复制文件夹']]) {
      const view = presentationModule.analysisCardPresentation(action, '来源/项目', '目标/项目');
      assert.equal(view.heading, heading);
      assert.deepEqual(view.paths, [
        { label: '复制自', path: '来源/项目' },
        { label: '复制到', path: '目标/项目' },
      ]);
      assert.match(view.basis, /不发送给模型/);
      assert.equal(view.createsPlanOnly, true);
    }
  });

  it('压缩包确认只展示本机处理的来源和目标', () => {
    const create = presentationModule.analysisCardPresentation('create_archive', '资料/合同', '归档/合同.zip');
    assert.deepEqual(create.paths, [
      { label: '打包来源', path: '资料/合同' },
      { label: '压缩包路径', path: '归档/合同.zip' },
    ]);
    assert.match(create.basis, /不发送给模型/);
    assert.equal(create.createsPlanOnly, true);

    const extract = presentationModule.analysisCardPresentation('extract_archive', '归档/合同.zip', '');
    assert.equal(extract.heading, '确认解压压缩包');
    assert.deepEqual(extract.paths, [
      { label: '压缩包', path: '归档/合同.zip' },
      { label: '解压到', path: '根目录' },
    ]);
    assert.match(extract.notice, /解压清单将在审批方案中展示/);
  });

  it('软链接确认卡显示服务端预期路径，并明确确认不会立即创建链接', () => {
    const view = presentationModule.analysisCardPresentation({
      action: 'create_symlink',
      goal: '为合同建立快捷入口',
      filePath: '资料/合同.pdf',
      targetPath: '快捷入口/合同 (1).pdf',
      operations: [{ action: 'create_symlink', filePath: '资料/合同.pdf', targetPath: '快捷入口/合同 (1).pdf' }],
      dataTypes: ['任务描述与路径'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    });

    assert.equal(view.heading, '确认创建软链接');
    assert.equal(view.goal, '为合同建立快捷入口');
    assert.deepEqual(view.operations[0].paths, [
      { label: '指向', path: '资料/合同.pdf' },
      { label: '新链接路径', path: '快捷入口/合同 (1).pdf' },
    ]);
    assert.match(view.notice, /不会创建软链接/);
    assert.equal(view.confirmLabel, '确认并生成方案');
    assert.equal(view.createsPlanOnly, true);
  });

  it('完整显示多项确认操作与共享的数据范围', () => {
    const view = presentationModule.analysisCardPresentation({
      goal: '将合同按年份归档',
      operations: [
        { action: 'create_directory', filePath: '合同', targetPath: '合同/2025' },
        { action: 'copy_file', filePath: '待整理/说明.pdf', targetPath: '合同/2025/说明.pdf' },
        { action: 'copy_directory', filePath: '待整理/项目', targetPath: '合同/2025/项目' },
        { action: 'move_file', filePath: '待整理/合同.pdf', targetPath: '合同/2025/合同.pdf' },
        { action: 'set_permissions', filePath: '合同/2025/合同.pdf', targetPath: '', requestedMode: 292 },
      ],
      dataTypes: ['任务描述', '文件名与修改日期'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    });
    assert.equal(view.heading, '确认任务与数据范围');
    assert.equal(view.goal, '将合同按年份归档');
    assert.deepEqual(view.operations.map(({ title }) => title), ['新建文件夹', '复制文件', '复制文件夹', '移动文件', '修改权限']);
    assert.deepEqual(view.operations[1].paths, [
      { label: '复制自', path: '待整理/说明.pdf' },
      { label: '复制到', path: '合同/2025/说明.pdf' },
    ]);
    assert.deepEqual(view.operations[2].paths, [
      { label: '复制自', path: '待整理/项目' },
      { label: '复制到', path: '合同/2025/项目' },
    ]);
    assert.deepEqual(view.operations[3].paths, [
      { label: '原路径', path: '待整理/合同.pdf' },
      { label: '目标路径', path: '合同/2025/合同.pdf' },
    ]);
    assert.deepEqual(view.operations[4].metadata, [{ label: '目标权限', value: '0444' }]);
    assert.deepEqual(view.dataTypes, ['任务描述', '文件名与修改日期']);
    assert.equal(view.provider, 'ollama');
    assert.equal(view.model, 'qwen3:8b');
    assert.equal(view.destination, '本地');
    assert.equal(view.createsPlanOnly, true);
  });

  it('照片归档确认卡只显示范围、目标和本机拍摄元数据读取说明', () => {
    const view = presentationModule.analysisCardPresentation({
      action: 'archive_photos',
      goal: '按拍摄日期归档照片',
      filePath: '相机',
      targetPath: '照片',
      dataTypes: ['本机拍摄日期元数据'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    });
    assert.deepEqual(view, {
      heading: '确认照片归档',
      goal: '按拍摄日期归档照片',
      scopePath: '相机',
      targetPath: '照片',
      dataUse: '只读取服务器本机的拍摄日期元数据；不读取或发送图片正文。',
      notice: '确认后生成逐项待审核方案，不会移动文件。',
      confirmLabel: '确认并生成方案',
      createsPlanOnly: true,
    });
    const rootView = presentationModule.analysisCardPresentation({
      action: 'archive_photos', goal: '按拍摄日期归档照片', filePath: '', targetPath: '照片',
    });
    assert.equal(rootView.scopePath, '管理根目录');
  });

  it('正文修改确认卡明确列出将读取的文件全文', () => {
    const view = presentationModule.analysisCardPresentation({
      goal: '修正文档中的错别字',
      operations: [{ action: 'edit_text', filePath: '文档/说明.md', targetPath: '' }],
      dataTypes: ['任务描述', 'UTF-8 文件正文全文'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    });
    assert.deepEqual(view.operations, [{
      number: 1,
      title: '修改文件正文',
      paths: [{ label: '读取正文（全文）', path: '文档/说明.md' }],
      metadata: [],
    }]);
    assert.match(view.notice, /读取所列文件正文全文/);
    assert.equal(view.createsPlanOnly, false);
    assert.equal(view.confirmLabel, '确认并分析');
  });

  it('Shell 确认卡展示完整命令与范围，只说明会在隔离区运行', () => {
    const command = 'find . -type f -name "*.tmp" -print0 | xargs -0 -r rm --';
    const view = presentationModule.analysisCardPresentation({
      action: 'run_shell',
      goal: '清理临时文件',
      filePath: '',
      shellCommand: command,
      workingDir: '',
    });
    assert.equal(view.heading, '确认隔离运行');
    assert.equal(view.scopePath, '管理根目录');
    assert.equal(view.workingDir, '管理根目录');
    assert.equal(view.shellCommand, command);
    assert.match(view.notice, /隔离工作区运行/);
    assert.match(view.notice, /批准并再次确认/);
    assert.equal(view.confirmLabel, '运行并查看变化');
    assert.match(source, /className="agent-shell-command">\{analysisCardView\.shellCommand\}<\/pre>/);
    assert.match(styles, /\.agent-shell-command\s*\{/);
  });

  it('正文分类确认卡展示递归来源、候选文件、已有类别和正文发送范围', () => {
    const view = presentationModule.analysisCardPresentation({
      action: 'classify_documents',
      goal: '按正文主题归入已有类别',
      filePath: '待整理',
      targetPath: '分类库',
      operations: [{
        action: 'classify_documents',
        filePath: '待整理',
        targetPath: '分类库',
        files: [
          { path: '待整理/项目/合同.md', sizeBytes: 1200 },
          { path: '待整理/报告.txt', sizeBytes: 86 },
        ],
        categories: [
          { id: 0, path: '分类库/合同' },
          { id: 1, path: '分类库/报告' },
        ],
      }],
      dataTypes: ['所列文件正文全文'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    });

    assert.equal(view.heading, '确认按正文分类');
    assert.equal(view.scopePath, '待整理');
    assert.equal(view.targetPath, '分类库');
    assert.deepEqual(view.files, [
      { path: '待整理/项目/合同.md', sizeBytes: 1200, sizeLabel: '1.2 KB' },
      { path: '待整理/报告.txt', sizeBytes: 86, sizeLabel: '86 B' },
    ]);
    assert.deepEqual(view.categories, ['分类库/合同', '分类库/报告']);
    assert.equal(view.modelSummary, 'openai-compatible · glm-5.3-flash · 远程');
    assert.match(view.dataUse, /PDF／Office 最多读取前 20 页/);
    assert.match(view.notice, /逐项待审核/);
    assert.equal(view.confirmLabel, '确认并分析正文');
    assert.equal(view.createsPlanOnly, false);
    assert.match(source, /递归范围/);
    assert.match(source, /确认后读取的文件/);
    assert.match(source, /已有分类目录/);
    assert.match(source, /analysisCardView\.modelSummary/);
    assert.match(source, /每次最多 2 份 PDF／Office/);
    assert.match(source, /ocrBatchLimitExceeded/);
    assert.match(styles, /\.agent-classification-lists\s*\{/);
    assert.match(styles, /@media \(max-width: 440px\)[\s\S]*?\.agent-classification-lists\s*\{\s*grid-template-columns: minmax\(0, 1fr\);/);
  });

  it('权限任务先确认路径与目标值，明确不读取文件内容', () => {
    const view = presentationModule.analysisCardPresentation('set_permissions', '文档/报告.pdf', '', 292);
    assert.equal(view.heading, '确认权限变更');
    assert.deepEqual(view.paths, [{ label: '文件', path: '文档/报告.pdf' }]);
    assert.deepEqual(view.metadata, [{ label: '目标权限', value: '0444' }]);
    assert.match(view.basis, /不读取文件内容/);
    assert.equal(view.createsPlanOnly, true);
  });

  it('所有者任务只列出明确请求的 UID 与 GID', () => {
    const view = presentationModule.analysisCardPresentation('set_owner', '文档/报告.pdf', '', undefined, 0, 1000);
    assert.deepEqual(view.metadata, [
      { label: '目标用户 ID', value: '0' },
      { label: '目标用户组 ID', value: '1000' },
    ]);
    assert.match(view.basis, /不读取文件内容/);
  });

  it('重复扫描确认卡展示范围、模型与文件数据去向', () => {
    const view = presentationModule.analysisCardPresentation({
      action: 'find_duplicates',
      goal: '找出资料目录内内容相同的文件',
      filePath: '资料',
      targetPath: '',
      dataTypes: ['文件路径、大小与服务端内容摘要'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    });
    assert.deepEqual(view, {
      heading: '确认重复文件扫描',
      goal: '找出资料目录内内容相同的文件',
      scopePath: '资料',
      modelSummary: 'ollama · qwen3:8b · 本地',
      dataUse: '任务描述和范围会发送给此模型；文件内容只在 FileSage 服务器本地计算 SHA-256，不会发送给模型。',
      notice: '只读扫描，不会修改文件。',
      confirmLabel: '开始扫描',
      isDuplicateScan: true,
    });
  });

  it('扫描报告明确可释放空间只是估算值', () => {
    assert.match(duplicateReportSource, /可释放空间估算/);
    assert.match(duplicateReportSource, /先选择保留文件，再勾选要移入回收站的副本/);
    assert.match(duplicateReportSource, /createDuplicateCleanupPlan/);
    assert.match(duplicateReportSource, /移入回收站/);
  });
});
