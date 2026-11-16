import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from "react";
import * as monaco from "monaco-editor";
import EditorWorker from "monaco-editor/editor/editor.worker.js?worker";
import { agentApi } from "../api";
import { connect, repoUri, type LanguageClient } from "./lsp";

self.MonacoEnvironment = { getWorker: () => new EditorWorker() };

const token = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();

// applyTheme builds Monaco's theme from the design tokens, so the editor
// follows light and dark like the rest of the app.
function applyTheme() {
  const hex = (name: string) => token(name).replace("#", "");
  const background = token("window");
  const dark = parseInt(background.slice(1, 3), 16) < 128;
  monaco.editor.defineTheme("stackwright", {
    base: dark ? "vs-dark" : "vs",
    inherit: true,
    rules: [
      { token: "keyword", foreground: hex("syntax-keyword") },
      { token: "type", foreground: hex("syntax-keyword") },
      { token: "string", foreground: hex("syntax-string") },
      { token: "number", foreground: hex("syntax-number") },
      { token: "comment", foreground: hex("ink-muted") },
      { token: "identifier", foreground: hex("ink") },
      { token: "variable", foreground: hex("ink") },
    ],
    colors: {
      "editor.background": background,
      "editor.foreground": token("ink"),
      "editorLineNumber.foreground": token("ink-muted"),
      "editor.selectionBackground": token("selection"),
      "editorWidget.background": token("chrome"),
      "editorWidget.border": token("hairline"),
    },
  });
  monaco.editor.setTheme("stackwright");
}

const languageOf = (path: string) => (/\.(tf|tfvars|hcl)$/.test(path) ? "hcl" : "plaintext");

// rev counts changes to text from outside the editor: an open, a save's
// result or a reload. Typing never changes it.
export type OpenFile = { path: string; text: string; rev: number };

type Props = {
  org: string;
  session: string;
  files: OpenFile[];
  active: string;
  onChange: (path: string, text: string) => void;
  onSave: () => void;
};

export type CodeHandle = {
  // format runs tofu-ls's formatting on a file and returns its text.
  format: (path: string) => Promise<string>;
  focus: () => void;
};

export const Code = forwardRef<CodeHandle, Props>(function Code({ org, session, files, active, onChange, onSave }, ref) {
  const host = useRef<HTMLDivElement>(null);
  const editor = useRef<monaco.editor.IStandaloneCodeEditor>(undefined);
  const client = useRef<LanguageClient>(undefined);
  const applied = useRef(new Map<string, number>());
  const handlers = useRef({ onChange, onSave });
  handlers.current = { onChange, onSave };
  const [language, setLanguage] = useState<"connecting" | "on" | "off">("connecting");

  useEffect(() => {
    applyTheme();
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const watch = new MutationObserver(applyTheme);
    watch.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    media.addEventListener("change", applyTheme);
    const e = monaco.editor.create(host.current!, {
      theme: "stackwright",
      automaticLayout: true,
      minimap: { enabled: false },
      fontFamily: token("font-mono") || undefined,
      ariaLabel: "File text",
      model: null,
    });
    e.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => handlers.current.onSave());
    e.onDidChangeModelContent(() => {
      const model = e.getModel();
      if (!model) return;
      client.current?.change(model);
      handlers.current.onChange(model.uri.path.replace(/^\/repo\//, ""), model.getValue());
    });
    editor.current = e;
    return () => {
      watch.disconnect();
      media.removeEventListener("change", applyTheme);
      e.dispose();
      for (const m of monaco.editor.getModels()) m.dispose();
    };
  }, []);

  // The language server reconnects after it drops, with a new ticket.
  useEffect(() => {
    let stopped = false;
    let retry: ReturnType<typeof setTimeout>;
    const open = async () => {
      setLanguage("connecting");
      try {
        const { ticket } = await agentApi.openLanguageServer({ org, id: session });
        if (stopped) return;
        const scheme = window.location.protocol === "https:" ? "wss" : "ws";
        client.current = connect(monaco, `${scheme}://${window.location.host}/lsp/${session}`, ticket, () => {
          client.current?.dispose();
          client.current = undefined;
          setLanguage("off");
          if (!stopped) retry = setTimeout(open, 5000);
        });
        for (const m of monaco.editor.getModels()) client.current.open(m);
        setLanguage("on");
      } catch {
        setLanguage("off");
        if (!stopped) retry = setTimeout(open, 5000);
      }
    };
    void open();
    return () => {
      stopped = true;
      clearTimeout(retry);
      client.current?.dispose();
      client.current = undefined;
    };
  }, [org, session]);

  // Models follow the open files: one for each, and its text when a save
  // or a reload changes it from outside.
  useEffect(() => {
    const wanted = new Set(files.map((f) => repoUri(f.path)));
    for (const m of monaco.editor.getModels()) {
      if (!wanted.has(m.uri.toString())) {
        client.current?.close(m);
        m.dispose();
      }
    }
    for (const f of files) {
      const uri = monaco.Uri.parse(repoUri(f.path));
      const model = monaco.editor.getModel(uri);
      if (!model) {
        const created = monaco.editor.createModel(f.text, languageOf(f.path), uri);
        client.current?.open(created);
      } else if (applied.current.get(f.path) !== f.rev && model.getValue() !== f.text) {
        model.pushEditOperations([], [{ range: model.getFullModelRange(), text: f.text }], () => null);
      }
      applied.current.set(f.path, f.rev);
    }
    const model = active ? monaco.editor.getModel(monaco.Uri.parse(repoUri(active))) : null;
    if (editor.current && editor.current.getModel() !== model) editor.current.setModel(model);
  }, [files, active]);

  useImperativeHandle(ref, () => ({
    format: async (path) => {
      const model = monaco.editor.getModel(monaco.Uri.parse(repoUri(path)));
      if (!model) return "";
      const e = editor.current;
      if (e && e.getModel() === model) await e.getAction("editor.action.formatDocument")?.run();
      return model.getValue();
    },
    focus: () => editor.current?.focus(),
  }));

  return (
    <div className="code">
      <div ref={host} className="code-host" />
      <p className="code-status muted" role="status">
        {language === "on" ? "Language support on" : language === "connecting" ? "Language support connecting…" : "Language support off; retrying"}
      </p>
    </div>
  );
});

export default Code;
