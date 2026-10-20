// 本组件只维护 CodeMirror 生命周期，草稿和保存仍由阅读面板管理。
import { useEffect, useRef } from 'react';
import { Compartment, EditorState } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { openSearchPanel } from '@codemirror/search';
import { Search } from 'lucide-react';
import { textEditorExtensions, textReaderExtensions, languageSupportFor } from './textEditorConfig';

export interface TextCodeEditorProps {
  name: string;
  value: string;
  disabled: boolean;
  readOnly?: boolean;
  onChange?: (value: string) => void;
}

// 创建可销毁的编辑器实例，并同步外部草稿与忙碌状态。
export function TextCodeEditor({ name, value, disabled, readOnly = false, onChange }: TextCodeEditorProps) {
  const mountRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const syncingValueRef = useRef(false);
  const compartmentsRef = useRef({
    editable: new Compartment(),
    readOnly: new Compartment(),
    mode: new Compartment(),
    language: new Compartment(),
    label: new Compartment(),
  });
  onChangeRef.current = onChange;

  useEffect(() => {
    const parent = mountRef.current;
    if (!parent) return;

    const compartments = compartmentsRef.current;
    const view = new EditorView({
      parent,
      state: EditorState.create({
        doc: value,
        extensions: [
          compartments.mode.of(readOnly ? textReaderExtensions() : textEditorExtensions()),
          compartments.editable.of(EditorView.editable.of(!disabled && !readOnly)),
          compartments.readOnly.of(EditorState.readOnly.of(disabled || readOnly)),
          compartments.language.of([]),
          compartments.label.of(EditorView.contentAttributes.of({
            'aria-label': `${name} 的 UTF-8 ${readOnly ? '文本阅读器' : '文本编辑器'}`,
            spellcheck: 'false',
          })),
          EditorView.updateListener.of((update) => {
            if (update.docChanged && !syncingValueRef.current) {
              onChangeRef.current?.(update.state.doc.toString());
            }
          }),
        ],
      }),
    });
    viewRef.current = view;

    return () => {
      view.destroy();
      if (viewRef.current === view) viewRef.current = null;
    };
  }, []);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;

    const compartments = compartmentsRef.current;
    view.dispatch({
      effects: [
        compartments.mode.reconfigure(readOnly ? textReaderExtensions() : textEditorExtensions()),
        compartments.editable.reconfigure(EditorView.editable.of(!disabled && !readOnly)),
        compartments.readOnly.reconfigure(EditorState.readOnly.of(disabled || readOnly)),
      ],
    });
  }, [disabled, readOnly]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view || view.state.doc.toString() === value) return;

    syncingValueRef.current = true;
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } });
    syncingValueRef.current = false;
  }, [value]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;

    const compartments = compartmentsRef.current;
    let active = true;
    view.dispatch({ effects: compartments.language.reconfigure([]) });
    view.dispatch({
      effects: compartments.label.reconfigure(EditorView.contentAttributes.of({
        'aria-label': `${name} 的 UTF-8 ${readOnly ? '文本阅读器' : '文本编辑器'}`,
        spellcheck: 'false',
      })),
    });

    void languageSupportFor(name)
      .then((languageSupport) => {
        if (active && viewRef.current === view) {
          view.dispatch({ effects: compartments.language.reconfigure(languageSupport ?? []) });
        }
      })
      .catch(() => {
        // 解析器资源加载失败时保留纯文本编辑能力。
      });

    return () => {
      active = false;
    };
  }, [name, readOnly]);

  return (
    <div className="reader-code-viewer">
      {readOnly && (
        <div className="reader-code-viewer-actions">
          <button
            className="reader-action-button"
            type="button"
            onClick={() => {
              const view = viewRef.current;
              if (view) openSearchPanel(view);
            }}
          >
            <Search size={16} aria-hidden="true" />查找
          </button>
        </div>
      )}
      <div
        ref={mountRef}
        className={`reader-text-editor${disabled ? ' reader-text-editor-disabled' : ''}`}
        aria-label={`${name} 的 UTF-8 ${readOnly ? '文本阅读器' : '文本编辑器'}`}
      />
    </div>
  );
}
