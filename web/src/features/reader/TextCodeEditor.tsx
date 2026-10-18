// 本组件只维护 CodeMirror 生命周期，草稿和保存仍由阅读面板管理。
import { useEffect, useRef } from 'react';
import { Compartment, EditorState } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { textEditorExtensions, languageSupportFor } from './textEditorConfig';

export interface TextCodeEditorProps {
  name: string;
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
}

// 创建可销毁的编辑器实例，并同步外部草稿与忙碌状态。
export function TextCodeEditor({ name, value, disabled, onChange }: TextCodeEditorProps) {
  const mountRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const syncingValueRef = useRef(false);
  const compartmentsRef = useRef({
    editable: new Compartment(),
    readOnly: new Compartment(),
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
          ...textEditorExtensions(),
          compartments.editable.of(EditorView.editable.of(!disabled)),
          compartments.readOnly.of(EditorState.readOnly.of(disabled)),
          compartments.language.of([]),
          compartments.label.of(EditorView.contentAttributes.of({
            'aria-label': `${name} 的 UTF-8 文本编辑器`,
            spellcheck: 'false',
          })),
          EditorView.updateListener.of((update) => {
            if (update.docChanged && !syncingValueRef.current) {
              onChangeRef.current(update.state.doc.toString());
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
        compartments.editable.reconfigure(EditorView.editable.of(!disabled)),
        compartments.readOnly.reconfigure(EditorState.readOnly.of(disabled)),
      ],
    });
  }, [disabled]);

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
        'aria-label': `${name} 的 UTF-8 文本编辑器`,
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
  }, [name]);

  return (
    <div
      ref={mountRef}
      className={`reader-text-editor${disabled ? ' reader-text-editor-disabled' : ''}`}
      aria-label={`${name} 的 UTF-8 文本编辑器`}
    />
  );
}
