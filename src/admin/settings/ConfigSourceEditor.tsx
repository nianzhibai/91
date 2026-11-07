import { useMemo, type Ref } from "react";
import CodeMirror, {
  EditorView,
  type ReactCodeMirrorRef,
} from "@uiw/react-codemirror";
import { yaml } from "@codemirror/lang-yaml";
import { useCurrentTheme } from "@/lib/useCurrentTheme";

type Props = {
  editorRef?: Ref<ReactCodeMirrorRef>;
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
};

export default function ConfigSourceEditor({
  editorRef,
  value,
  disabled,
  onChange,
}: Props) {
  const activeTheme = useCurrentTheme();
  const theme = activeTheme === "dark" ? "dark" : "light";
  const extensions = useMemo(
    () => [yaml(), EditorView.contentAttributes.of({ "aria-label": "config.yaml 源码" })],
    []
  );

  return (
    <CodeMirror
      ref={editorRef}
      className="admin-config-source__codemirror"
      value={value}
      height="100%"
      theme={theme}
      extensions={extensions}
      editable={!disabled}
      readOnly={disabled}
      placeholder="在此编辑 config.yaml"
      onChange={onChange}
      basicSetup={{
        lineNumbers: true,
        highlightActiveLineGutter: true,
        highlightActiveLine: true,
        foldGutter: true,
        dropCursor: true,
        allowMultipleSelections: true,
        indentOnInput: true,
        bracketMatching: true,
        closeBrackets: true,
        autocompletion: false,
        rectangularSelection: true,
        crosshairCursor: false,
        highlightSelectionMatches: true,
        closeBracketsKeymap: true,
        searchKeymap: true,
        foldKeymap: true,
        completionKeymap: false,
        lintKeymap: true,
      }}
    />
  );
}
