import assert from 'node:assert/strict';
import { afterEach, describe, it } from 'node:test';
import {
  AgentConversationApiError,
  canConfirmAgentAnalysis,
  canSendAgentMessage,
  confirmAgentAnalysis,
  createAgentConversation,
  listAgentConversations,
  parseAgentConversation,
  parseAgentConversationPage,
  parseAgentModelStatus,
  sendAgentConversationMessage,
} from '../src/api/agentConversation.ts';

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function conversation(overrides = {}) {
  return {
    id: 'conv-1',
    state: 'awaiting_confirmation',
    messages: [{ role: 'user', content: 'rename report.txt' }],
    analysisCard: {
      action: 'rename',
      goal: '按正文标题重命名',
      filePath: '文档/report.txt',
      targetPath: '',
      dataTypes: ['UTF-8 文件正文全文'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    },
    taskId: '',
    planId: '',
    error: '',
    ...overrides,
  };
}

describe('Agent 对话 API 解析', () => {
  it('解析本地模型状态并隐藏不可用时的凭据字段', () => {
    assert.deepEqual(parseAgentModelStatus({
      available: true,
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    }), {
      available: true,
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    });
    assert.deepEqual(parseAgentModelStatus({ available: false, reason: 'not_configured', apiKey: 'secret' }), {
      available: false,
      reason: 'not_configured',
    });
  });

  it('拒绝不完整的模型状态、消息和分析卡', () => {
    assert.throws(() => parseAgentModelStatus({ available: true, provider: 'ollama' }), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({ messages: [{ role: 'tool', content: 'x' }] })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...conversation().analysisCard, dataTypes: ['正文', 42] } })), AgentConversationApiError);
  });

  it('保留新建文件夹确认卡的动作和目标路径', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'create_directory',
        goal: '按年份整理照片',
        filePath: '',
        targetPath: '照片/2025',
        dataTypes: ['任务描述与目标路径'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'create_directory');
    assert.equal(parsed.analysisCard.targetPath, '照片/2025');
    assert.equal(parsed.analysisCard.filePath, '');
  });

  it('解析普通删除确认卡并要求目标路径为空', () => {
    const card = {
      action: 'trash',
      goal: '清理旧文件',
      filePath: '文档/旧稿.md',
      targetPath: '',
      dataTypes: ['任务描述与路径，不读取正文'],
      provider: 'test-provider',
      model: 'test-model',
      destination: 'remote',
    };
    const parsed = parseAgentConversation(conversation({ analysisCard: card }));
    assert.equal(parsed.analysisCard.action, 'trash');
    assert.equal(parsed.analysisCard.filePath, '文档/旧稿.md');
    assert.equal(parsed.analysisCard.targetPath, '');
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...card, targetPath: '其他/旧稿.md' } })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...card, filePath: '../外部/旧稿.md' } })), AgentConversationApiError);
  });

  it('解析永久删除确认卡并要求目标路径为空', () => {
    const card = {
      action: 'permanent_delete',
      goal: '永久删除过期文件',
      filePath: '文档/过期.txt',
      targetPath: '',
      dataTypes: ['任务描述与路径，不读取正文'],
      provider: 'test-provider',
      model: 'test-model',
      destination: 'remote',
    };
    const parsed = parseAgentConversation(conversation({ analysisCard: card }));
    assert.equal(parsed.analysisCard.action, 'permanent_delete');
    assert.equal(parsed.analysisCard.filePath, '文档/过期.txt');
    assert.equal(parsed.analysisCard.targetPath, '');
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...card, targetPath: '恢复/过期.txt' } })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...card, filePath: '../外部/过期.txt' } })), AgentConversationApiError);
  });

  it('解析单文件移动确认卡的源路径与目标路径', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'move_file',
        goal: '将合同归档到合同文件夹',
        filePath: '待整理/合同.pdf',
        targetPath: '合同/合同.pdf',
        dataTypes: ['任务描述、源路径与目标路径'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'move_file');
    assert.equal(parsed.analysisCard.filePath, '待整理/合同.pdf');
    assert.equal(parsed.analysisCard.targetPath, '合同/合同.pdf');
  });

  it('解析软链接确认卡的目标对象与最终链接路径，并拒绝越界或不匹配路径', () => {
    const operation = {
      action: 'create_symlink',
      filePath: '资料/合同.pdf',
      targetPath: '快捷入口/合同 (1).pdf',
    };
    const card = {
      action: 'create_symlink',
      goal: '在快捷入口创建合同链接',
      filePath: operation.filePath,
      targetPath: operation.targetPath,
      operations: [operation],
      dataTypes: ['任务描述与路径'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    };
    const parsed = parseAgentConversation(conversation({ analysisCard: card }));
    assert.equal(parsed.analysisCard.action, 'create_symlink');
    assert.equal(parsed.analysisCard.filePath, '资料/合同.pdf');
    assert.equal(parsed.analysisCard.targetPath, '快捷入口/合同 (1).pdf');
    assert.deepEqual(parsed.analysisCard.operations, [operation]);

    for (const invalid of [
      { ...card, filePath: '../外部/合同.pdf' },
      { ...card, targetPath: '/快捷入口/合同.pdf' },
      { ...card, operations: [{ ...operation, targetPath: '其他/合同.pdf' }] },
      { ...card, operations: [operation, operation] },
    ]) {
      assert.throws(() => parseAgentConversation(conversation({ analysisCard: invalid })), AgentConversationApiError);
    }
  });

  it('解析拍摄照片归档确认卡的来源范围和目标目录', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'archive_photos',
        goal: '按拍摄日期归档照片',
        filePath: '相机',
        targetPath: '照片',
        operations: [{ action: 'archive_photos', filePath: '相机', targetPath: '照片' }],
        dataTypes: ['本机拍摄日期元数据'],
        provider: 'openai-compatible',
        model: 'glm-5.3-flash',
        destination: 'remote',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'archive_photos');
    assert.equal(parsed.analysisCard.filePath, '相机');
    assert.equal(parsed.analysisCard.targetPath, '照片');
    assert.equal(parsed.analysisCard.operations, undefined);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        ...parsed.analysisCard,
        operations: [{ action: 'archive_photos', filePath: '其他', targetPath: '照片' }],
      },
    })), AgentConversationApiError);
  });

  it('解析唯一的正文分类确认操作及其文件和类别清单', () => {
    const operation = {
      action: 'classify_documents',
      filePath: '待整理',
      targetPath: '分类库',
      files: [
        { path: '待整理/合同/供货.md', sizeBytes: 1200 },
        { path: '待整理/报告.txt', sizeBytes: 86 },
        { path: '待整理/扫描报告.pdf', sizeBytes: 15 * 1024 * 1024 },
      ],
      categories: [
        { id: 0, path: '分类库/合同' },
        { id: 1, path: '分类库/报告' },
      ],
    };
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'classify_documents',
        goal: '按正文主题归入已有类别',
        filePath: '待整理',
        targetPath: '分类库',
        operations: [operation],
        dataTypes: ['候选文件路径、大小和正文全文'],
        provider: 'openai-compatible',
        model: 'glm-5.3-flash',
        destination: 'remote',
      },
    }));

    assert.equal(parsed.analysisCard.action, 'classify_documents');
    assert.equal(parsed.analysisCard.filePath, '待整理');
    assert.equal(parsed.analysisCard.targetPath, '分类库');
    assert.deepEqual(parsed.analysisCard.operations, [operation]);
  });

  it('根目录可作为来源范围，分类卡必须与唯一分类操作完全匹配', () => {
    const operation = {
      action: 'classify_documents',
      filePath: '',
      targetPath: '分类库',
      files: [{ path: '说明.md', sizeBytes: 0 }],
      categories: [{ id: 0, path: '分类库/其他' }],
    };
    const card = {
      action: 'classify_documents',
      goal: '按正文归类',
      filePath: '',
      targetPath: '分类库',
      operations: [operation],
      dataTypes: ['UTF-8 正文全文'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    };
    assert.equal(parseAgentConversation(conversation({ analysisCard: card })).analysisCard.filePath, '');
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...card, operations: [operation, operation] },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...card, operations: [{ ...operation, targetPath: '其他' }] },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...card, operations: [{ ...operation, action: 'move_file' }] },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...card, action: 'move_file' },
    })), AgentConversationApiError);
  });

  it('拒绝畸形、越界或重复的分类文件与类别数据', () => {
    const operation = {
      action: 'classify_documents',
      filePath: '待整理',
      targetPath: '分类库',
      files: [{ path: '待整理/合同.md', sizeBytes: 1200 }],
      categories: [{ id: 0, path: '分类库/合同' }],
    };
    const card = {
      action: 'classify_documents',
      goal: '按正文归类',
      filePath: '待整理',
      targetPath: '分类库',
      operations: [operation],
      dataTypes: ['UTF-8 正文全文'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    };
    const invalidOperations = [
      { ...operation, files: [] },
      { ...operation, files: [{ path: '../机密.md', sizeBytes: 100 }] },
      { ...operation, files: [{ path: '其他/机密.md', sizeBytes: 100 }] },
      { ...operation, files: [{ path: '待整理/合同.md', sizeBytes: -1 }] },
      { ...operation, files: [{ path: '待整理/合同.md', sizeBytes: 1.5 }] },
      { ...operation, files: [{ path: '待整理/合同.md', sizeBytes: 64 * 1024 + 1 }] },
      { ...operation, files: [operation.files[0], operation.files[0]] },
      { ...operation, categories: [] },
      { ...operation, categories: [{ id: -1, path: '分类库/合同' }] },
      { ...operation, categories: [{ id: 50, path: '分类库/合同' }] },
      { ...operation, categories: [{ id: 1.5, path: '分类库/合同' }] },
      { ...operation, categories: [{ id: '0', path: '分类库/合同' }] },
      { ...operation, categories: [{ id: 0, path: '../合同' }] },
      { ...operation, categories: [{ id: 0, path: '分类库' }] },
      { ...operation, categories: [operation.categories[0], operation.categories[0]] },
      { ...operation, categories: [{ id: 0, path: '分类库/合同' }, { id: 0, path: '分类库/报告' }] },
      { ...operation, categories: [{ id: 0, path: '分类库/合同' }, { id: 1, path: '分类库/合同' }] },
    ];

    for (const invalidOperation of invalidOperations) {
      assert.throws(() => parseAgentConversation(conversation({
        analysisCard: { ...card, operations: [invalidOperation] },
      })), AgentConversationApiError);
    }

    const boundaryOperation = {
      ...operation,
      files: [{ path: '待整理/合同.md', sizeBytes: 64 * 1024 }],
      categories: [{ id: 49, path: '分类库/合同' }],
    };
    assert.deepEqual(
      parseAgentConversation(conversation({ analysisCard: { ...card, operations: [boundaryOperation] } }))
        .analysisCard.operations[0],
      boundaryOperation,
    );
  });

  it('不接受分类数据挂在其他动作或多项操作清单中', () => {
    const classifyOperation = {
      action: 'classify_documents',
      filePath: '待整理',
      targetPath: '分类库',
      files: [{ path: '待整理/报告.md', sizeBytes: 12 }],
      categories: [{ id: 'cat-report', path: '分类库/报告' }],
    };
    const common = {
      goal: '整理文件',
      dataTypes: ['正文全文'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    };
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        ...common,
        operations: [classifyOperation, { action: 'move_file', filePath: 'a', targetPath: 'b' }],
      },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        ...common,
        action: 'move_file',
        filePath: 'a',
        targetPath: 'b',
        operations: [{ ...classifyOperation, files: undefined, categories: undefined }],
      },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        ...common,
        action: 'move_file',
        filePath: 'a',
        targetPath: 'b',
        operations: [{ action: 'move_file', filePath: 'a', targetPath: 'b', files: classifyOperation.files }],
      },
    })), AgentConversationApiError);
  });

  it('解析修改文件正文确认卡，并确认读取范围是指定文件全文', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'edit_text',
        goal: '修正文档中的错别字',
        filePath: '文档/说明.md',
        dataTypes: ['任务描述', 'UTF-8 文件正文全文'],
        provider: 'openai-compatible',
        model: 'glm-5.3-flash',
        destination: 'remote',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'edit_text');
    assert.equal(parsed.analysisCard.filePath, '文档/说明.md');
    assert.equal(parsed.analysisCard.targetPath, '');
    assert.deepEqual(parsed.analysisCard.dataTypes, ['任务描述', 'UTF-8 文件正文全文']);
  });

  it('解析新建与覆盖文本文件确认卡并严格限制为一个安全文本路径', () => {
    const common = {
      goal: '按要求准备文本文件',
      dataTypes: ['任务描述与目标路径'],
      provider: 'openai-compatible',
      model: 'glm-5.3-flash',
      destination: 'remote',
    };

    for (const action of ['create_file', 'overwrite_file']) {
      const filePath = action === 'create_file' ? '文档/新建.md' : '文档/说明.txt';
      const parsed = parseAgentConversation(conversation({
        analysisCard: {
          ...common,
          action,
          filePath,
          targetPath: '',
          operations: [{ action, filePath, targetPath: '' }],
        },
      }));
      assert.equal(parsed.analysisCard.action, action);
      assert.equal(parsed.analysisCard.filePath, filePath);
      assert.equal(parsed.analysisCard.targetPath, '');

      for (const invalid of [
        { filePath: '../越界.md' },
        { filePath: '文档/报告.pdf' },
        { targetPath: '其他/说明.md' },
        { operations: [{ action, filePath, targetPath: '' }, { action, filePath, targetPath: '' }] },
        { operations: [{ action: action === 'create_file' ? 'overwrite_file' : 'create_file', filePath, targetPath: '' }] },
      ]) {
        assert.throws(() => parseAgentConversation(conversation({
          analysisCard: { ...common, action, filePath, targetPath: '', operations: [{ action, filePath, targetPath: '' }], ...invalid },
        })), AgentConversationApiError);
      }
    }
  });

  it('解析服务端以单项操作返回的重复扫描确认卡', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'find_duplicates',
        goal: '查找 docs 中的重复文件',
        filePath: 'docs',
        targetPath: '',
        operations: [{ action: 'find_duplicates', filePath: 'docs', targetPath: '' }],
        dataTypes: ['任务描述与扫描范围'],
        provider: 'openai-compatible',
        model: 'glm-5.3-flash',
        destination: 'remote',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'find_duplicates');
    assert.equal(parsed.analysisCard.filePath, 'docs');
    assert.equal(parsed.analysisCard.targetPath, '');
    assert.equal(parsed.analysisCard.operations, undefined);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        ...parsed.analysisCard,
        operations: [{ action: 'find_duplicates', filePath: 'other', targetPath: '' }],
      },
    })), AgentConversationApiError);
  });

  it('解析文件与文件夹复制确认卡，并说明内容只在本机复制', () => {
    for (const [action, source, target] of [
      ['copy_file', '待整理/报告.pdf', '归档/报告.pdf'],
      ['copy_directory', '待整理/项目', '归档/项目'],
    ]) {
      const parsed = parseAgentConversation(conversation({
        analysisCard: {
          action,
          goal: '复制一份以便整理',
          filePath: source,
          targetPath: target,
          dataTypes: ['任务描述、源路径与目标路径；内容仅在本机复制，不发送给模型'],
          provider: 'ollama',
          model: 'qwen3:8b',
          destination: 'local',
        },
      }));
      assert.equal(parsed.analysisCard.action, action);
      assert.equal(parsed.analysisCard.filePath, source);
      assert.equal(parsed.analysisCard.targetPath, target);
    }
  });

  it('Shell 确认卡完整展示命令、管理范围和工作目录，并支持根目录', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'run_shell',
        goal: '归一化图片文件名',
        filePath: '照片/待整理',
        shellCommand: 'for file in *.jpg; do mv -- "$file" "${file,,}"; done',
        workingDir: '照片/待整理',
        dataTypes: ['任务描述、命令与路径'],
        provider: 'openai-compatible',
        model: 'glm-5.3-flash',
        destination: 'remote',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'run_shell');
    assert.equal(parsed.analysisCard.filePath, '照片/待整理');
    assert.equal(parsed.analysisCard.workingDir, '照片/待整理');
    assert.equal(parsed.analysisCard.shellCommand, 'for file in *.jpg; do mv -- "$file" "${file,,}"; done');

    const root = parseAgentConversation(conversation({
      analysisCard: {
        action: 'run_shell', goal: '检查根目录', filePath: '', shellCommand: 'find . -maxdepth 1 -type f', workingDir: '',
        dataTypes: ['任务描述、命令与路径'], provider: 'ollama', model: 'qwen3:8b', destination: 'local',
      },
    }));
    assert.equal(root.analysisCard.filePath, '');
    assert.equal(root.analysisCard.workingDir, '');
  });

  it('Shell 确认卡接受单一嵌套操作并校验它与卡片内容一致', () => {
    const card = {
      action: 'run_shell', goal: '检查变更', filePath: '资料', shellCommand: 'touch result.txt', workingDir: '资料',
      operations: [{ action: 'run_shell', filePath: '资料', targetPath: '', shellCommand: 'touch result.txt', workingDir: '资料' }],
      dataTypes: ['命令与范围'], provider: 'ollama', model: 'qwen3:8b', destination: 'local',
    };
    assert.equal(parseAgentConversation(conversation({ analysisCard: card })).analysisCard.workingDir, '资料');
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...card, operations: [{ ...card.operations[0], shellCommand: 'rm -rf .' }] },
    })), AgentConversationApiError);
  });

  it('Shell 确认卡拒绝不安全路径、空命令和混合操作', () => {
    const card = {
      action: 'run_shell', goal: '整理文件', filePath: '待整理', shellCommand: 'touch report.txt', workingDir: '待整理',
      dataTypes: ['命令与路径'], provider: 'ollama', model: 'qwen3:8b', destination: 'local',
    };
    for (const invalid of [
      { ...card, filePath: '../外部' },
      { ...card, workingDir: '/tmp' },
      { ...card, workingDir: '待整理/../外部' },
      { ...card, shellCommand: '   ' },
      { ...card, shellCommand: 'echo\0bad' },
      { ...card, operations: [{ action: 'run_shell', filePath: '待整理', targetPath: '', shellCommand: 'echo ok', workingDir: '其他' }] },
      { ...card, operations: [{ action: 'run_shell', filePath: '待整理', targetPath: '', shellCommand: 'echo ok', workingDir: '待整理' }, { action: 'rename', filePath: 'a', targetPath: 'b' }] },
    ]) {
      assert.throws(() => parseAgentConversation(conversation({ analysisCard: invalid })), AgentConversationApiError);
    }
  });

  it('解析单项压缩包创建与解压确认，并允许解压到管理根目录', () => {
    for (const [action, filePath, targetPath] of [
      ['create_archive', '资料/合同', '归档/合同.zip'],
      ['extract_archive', '备份/合同.zip', ''],
    ]) {
      const parsed = parseAgentConversation(conversation({
        analysisCard: {
          action,
          goal: '整理合同归档',
          filePath,
          targetPath,
          operations: [{ action, filePath, targetPath }],
          dataTypes: ['任务描述与路径；内容仅在本机处理'],
          provider: 'ollama',
          model: 'qwen3:8b',
          destination: 'local',
        },
      }));
      assert.equal(parsed.analysisCard.operations[0].action, action);
      assert.equal(parsed.analysisCard.operations[0].targetPath, targetPath);
    }
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: {
        action: 'extract_archive', goal: '解压', filePath: '../外部.zip', targetPath: '',
        dataTypes: ['路径'], provider: 'ollama', model: 'qwen3:8b', destination: 'local',
      },
    })), AgentConversationApiError);
  });

  it('解析多项确认卡并保留统一任务与数据范围', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        goal: '按年份归档照片并整理重复项',
        operations: [
          { action: 'create_directory', filePath: '照片', targetPath: '照片/2025' },
          { action: 'copy_file', filePath: '待整理/说明.pdf', targetPath: '照片/2025/说明.pdf' },
          { action: 'copy_directory', filePath: '待整理/相册', targetPath: '照片/2025/相册' },
          { action: 'move_file', filePath: '待整理/照片.jpg', targetPath: '照片/2025/照片.jpg' },
          { action: 'set_permissions', filePath: '照片/2025/照片.jpg', targetPath: '', requestedMode: 292 },
        ],
        dataTypes: ['任务描述', '文件名与拍摄日期'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
    }));
    assert.equal(parsed.analysisCard.operations.length, 5);
    assert.equal(parsed.analysisCard.operations[0].targetPath, '照片/2025');
    assert.equal(parsed.analysisCard.operations[4].requestedMode, 292);
    assert.deepEqual(parsed.analysisCard.dataTypes, ['任务描述', '文件名与拍摄日期']);
    assert.equal(parsed.analysisCard.action, undefined);
  });

  it('拒绝空或不完整的多项确认卡', () => {
    const base = {
      goal: '整理文件',
      dataTypes: ['任务描述'],
      provider: 'ollama',
      model: 'qwen3:8b',
      destination: 'local',
    };
    assert.throws(() => parseAgentConversation(conversation({ analysisCard: { ...base, operations: [] } })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...base, operations: [{ action: 'set_permissions', filePath: '报告.pdf', targetPath: '' }] },
    })), AgentConversationApiError);
  });

  it('确认权限方案只使用任务与路径，并以四位八进制显示目标权限', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'set_permissions',
        goal: '将报告设为只读',
        filePath: '文档/报告.pdf',
        targetPath: '',
        requestedMode: 292,
        requestedOwnerUid: null,
        requestedGroupGid: null,
        dataTypes: ['任务描述、文件路径与目标权限'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
    }));
    assert.equal(parsed.analysisCard.action, 'set_permissions');
    assert.equal(parsed.analysisCard.requestedMode, 292);
    assert.equal(parsed.analysisCard.requestedOwnerUid, undefined);
  });

  it('确认所有者方案支持只修改 UID 或 GID，并拒绝不完整目标', () => {
    const parsed = parseAgentConversation(conversation({
      analysisCard: {
        action: 'set_owner',
        goal: '指定文件所有者',
        filePath: '文档/报告.pdf',
        targetPath: '',
        requestedMode: null,
        requestedOwnerUid: 0,
        requestedGroupGid: null,
        dataTypes: ['任务描述、文件路径与目标所有者'],
        provider: 'ollama',
        model: 'qwen3:8b',
        destination: 'local',
      },
    }));
    assert.equal(parsed.analysisCard.requestedOwnerUid, 0);
    assert.equal(parsed.analysisCard.requestedGroupGid, undefined);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...parsed.analysisCard, requestedOwnerUid: undefined },
    })), AgentConversationApiError);
    assert.throws(() => parseAgentConversation(conversation({
      analysisCard: { ...parsed.analysisCard, requestedOwnerUid: 0x1_0000_0000 },
    })), AgentConversationApiError);
  });

  it('解析服务端分页摘要，不把消息正文带入历史列表', () => {
    assert.deepEqual(parseAgentConversationPage({
      conversations: [{
        id: 'conv-2',
        state: 'awaiting_approval',
        title: '整理合同',
        updatedAt: '2026-10-15T12:00:00Z',
        taskId: 'task-2',
        planId: 'plan-2',
        messages: [{ role: 'user', content: '不应进入摘要' }],
      }],
      limit: 20,
      offset: 0,
      hasMore: true,
    }), {
      conversations: [{
        id: 'conv-2',
        state: 'awaiting_approval',
        title: '整理合同',
        updatedAt: '2026-10-15T12:00:00Z',
        taskId: 'task-2',
        planId: 'plan-2',
      }],
      limit: 20,
      offset: 0,
      hasMore: true,
    });
    assert.throws(() => parseAgentConversationPage({ conversations: [{ id: 'x' }], limit: 101, offset: 0, hasMore: false }), AgentConversationApiError);
  });

  it('只允许契约中的对话阶段继续发送；仅有确认卡时可确认分析', () => {
    for (const state of ['clarifying', 'awaiting_confirmation', 'unsupported', 'failed']) {
      assert.equal(canSendAgentMessage(state), true, state);
    }
    for (const state of ['processing', 'analyzing', 'awaiting_approval']) {
      assert.equal(canSendAgentMessage(state), false, state);
    }
    assert.equal(canConfirmAgentAnalysis(parseAgentConversation(conversation())), true);
    assert.equal(canConfirmAgentAnalysis(parseAgentConversation(conversation({ analysisCard: null }))), false);
    assert.equal(canConfirmAgentAnalysis(parseAgentConversation(conversation({ state: 'clarifying' }))), false);
  });
});

describe('Agent 对话 API 请求', () => {
  it('创建会话只提交用户消息并接受 201 响应', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify(conversation()), { status: 201, headers: { 'Content-Type': 'application/json' } });
    };

    const result = await createAgentConversation('rename report.txt');
    assert.equal(request.url, '/api/v1/agent/conversations');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { message: 'rename report.txt' });
    assert.equal(result.id, 'conv-1');
  });

  it('通过分页摘要接口读取服务端会话历史', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify({ conversations: [], limit: 20, offset: 40, hasMore: false }), { status: 200 });
    };

    const page = await listAgentConversations(20, 40);
    assert.equal(request.url, '/api/v1/agent/conversations?limit=20&offset=40');
    assert.equal(request.init.method, undefined);
    assert.equal(page.offset, 40);
    assert.equal(page.hasMore, false);
  });

  it('拒绝创建接口的意外成功状态码', async () => {
    globalThis.fetch = async () => new Response(JSON.stringify(conversation()), { status: 200 });
    await assert.rejects(createAgentConversation('rename report.txt'), (error) => {
      assert.ok(error instanceof AgentConversationApiError);
      assert.equal(error.status, 200);
      return true;
    });
  });

  it('确认分析只发送 OCR 选项，不提前传文件正文', async () => {
    let request;
    globalThis.fetch = async (url, init) => {
      request = { url: String(url), init };
      return new Response(JSON.stringify(conversation({ state: 'analyzing', analysisCard: null })), { status: 200 });
    };

    await confirmAgentAnalysis('conv/1', true);
    assert.equal(request.url, '/api/v1/agent/conversations/conv%2F1/confirm-analysis');
    assert.equal(request.init.method, 'POST');
    assert.deepEqual(JSON.parse(request.init.body), { enableOCR: true });
    assert.equal(request.init.headers?.['Content-Type'], 'application/json');
  });

  it('把 409 状态冲突映射为可恢复的 API 错误', async () => {
    globalThis.fetch = async () => new Response(JSON.stringify({ error: '会话状态已变化' }), { status: 409 });
    await assert.rejects(sendAgentConversationMessage('conv-1', '继续'), (error) => {
      assert.ok(error instanceof AgentConversationApiError);
      assert.equal(error.status, 409);
      assert.equal(error.message, '会话状态已变化，请刷新后重新核对。');
      return true;
    });
  });
});
