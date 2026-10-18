// 本文件集中管理文本编辑器的语法映射与 CodeMirror 扩展。
import type { Extension } from '@codemirror/state';
import { bracketMatching, defaultHighlightStyle, indentOnInput, StreamLanguage, syntaxHighlighting } from '@codemirror/language';
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
import { search, searchKeymap } from '@codemirror/search';
import { highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers } from '@codemirror/view';

export const textEditorKeymap = [
  ...searchKeymap,
  indentWithTab,
  ...defaultKeymap,
  ...historyKeymap,
];

// 按服务端可编辑扩展白名单按需加载解析器，其他文件继续按纯文本处理。
export async function languageSupportFor(name: string): Promise<Extension | null> {
  const extension = name.split('.').at(-1)?.toLowerCase() ?? '';

  switch (extension) {
    case 'c':
    case 'cc':
    case 'cpp':
    case 'h':
    case 'hpp':
      return (await import('@codemirror/lang-cpp')).cpp();
    case 'css':
      return (await import('@codemirror/lang-css')).css();
    case 'scss':
      return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/sass')).sass);
    case 'go':
      return (await import('@codemirror/lang-go')).go();
    case 'htm':
    case 'html':
      return (await import('@codemirror/lang-html')).html();
    case 'java':
      return (await import('@codemirror/lang-java')).java();
    case 'js':
      return (await import('@codemirror/lang-javascript')).javascript();
    case 'jsx':
      return (await import('@codemirror/lang-javascript')).javascript({ jsx: true });
    case 'ts':
      return (await import('@codemirror/lang-javascript')).javascript({ typescript: true });
    case 'tsx':
      return (await import('@codemirror/lang-javascript')).javascript({ jsx: true, typescript: true });
    case 'json':
      return (await import('@codemirror/lang-json')).json();
    case 'md':
    case 'markdown':
    case 'mdown':
      return (await import('@codemirror/lang-markdown')).markdown();
    case 'py':
      return (await import('@codemirror/lang-python')).python();
    case 'bash':
    case 'sh':
      return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/shell')).shell);
    case 'ps1':
      return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/powershell')).powerShell);
    case 'cfg':
    case 'conf':
    case 'ini':
      return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/properties')).properties);
    case 'sql':
      return (await import('@codemirror/lang-sql')).sql();
    case 'toml':
      return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/toml')).toml);
    case 'svg':
    case 'xml':
      return (await import('@codemirror/lang-xml')).xml();
    case 'yaml':
    case 'yml':
      return (await import('@codemirror/lang-yaml')).yaml();
    default:
      return null;
  }
}

// 为编辑与阅读提供一致的行号、查找、撤销和语法高亮行为。
export function textEditorExtensions(languageSupport?: Extension | null): Extension[] {
  return [
    lineNumbers(),
    highlightActiveLineGutter(),
    highlightActiveLine(),
    history(),
    indentOnInput(),
    bracketMatching(),
    search(),
    syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
    keymap.of(textEditorKeymap),
    ...(languageSupport ? [languageSupport] : []),
  ];
}
