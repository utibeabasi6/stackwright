import type * as Monaco from "monaco-editor";

// A small LSP client for tofu-ls in the session's sandbox (adr/0034): the
// requests the editor uses, over the server's WebSocket relay. Files are
// named file:///repo/<path>.

type Pending = { resolve: (v: unknown) => void; reject: (e: Error) => void };
type Position = { line: number; character: number };
type Range = { start: Position; end: Position };
type TextEdit = { range: Range; newText: string };
type Diagnostic = { range: Range; severity?: number; message: string; source?: string };
type CompletionItem = {
  label: string;
  kind?: number;
  detail?: string;
  documentation?: string | { value: string };
  insertText?: string;
  textEdit?: TextEdit;
};
type Hover = { contents: string | { value: string } | ({ value: string } | string)[]; range?: Range };

export const repoUri = (path: string) => `file:///repo/${path}`;

const toRange = (monaco: typeof Monaco, r: Range) =>
  new monaco.Range(r.start.line + 1, r.start.character + 1, r.end.line + 1, r.end.character + 1);

const text = (v: string | { value: string }) => (typeof v === "string" ? v : v.value);

export type LanguageClient = {
  open: (model: Monaco.editor.ITextModel) => void;
  change: (model: Monaco.editor.ITextModel) => void;
  close: (model: Monaco.editor.ITextModel) => void;
  dispose: () => void;
};

// connect opens the WebSocket, sends the ticket, initializes tofu-ls and
// registers Monaco's providers for HCL. onClosed runs when the connection
// ends for any reason but dispose.
export function connect(monaco: typeof Monaco, url: string, ticket: string, onClosed: () => void): LanguageClient {
  const ws = new WebSocket(url);
  const pending = new Map<number, Pending>();
  const versions = new Map<string, number>();
  const queued: string[] = [];
  let next = 1;
  let initialized = false;
  let disposed = false;

  // Until tofu-ls answers initialize, everything else waits.
  const send = (msg: { method: string; id?: number; params: object }) => {
    const data = JSON.stringify({ jsonrpc: "2.0", ...msg });
    if (initialized || msg.method === "initialize") ws.send(data);
    else queued.push(data);
  };
  const request = (method: string, params: object) =>
    new Promise<unknown>((resolve, reject) => {
      const id = next++;
      pending.set(id, { resolve, reject });
      send({ id, method, params });
    });
  const notify = (method: string, params: object) => send({ method, params });

  ws.onopen = async () => {
    ws.send(ticket);
    await request("initialize", {
      processId: null,
      rootUri: "file:///repo",
      capabilities: {
        textDocument: {
          completion: { completionItem: { snippetSupport: false, documentationFormat: ["markdown", "plaintext"] } },
          hover: { contentFormat: ["markdown", "plaintext"] },
          publishDiagnostics: {},
          formatting: {},
        },
      },
    });
    ws.send(JSON.stringify({ jsonrpc: "2.0", method: "initialized", params: {} }));
    initialized = true;
    for (const data of queued.splice(0)) ws.send(data);
  };
  ws.onmessage = (e) => {
    const msg = JSON.parse(String(e.data)) as {
      id?: number;
      method?: string;
      result?: unknown;
      error?: { message: string };
      params?: { uri: string; diagnostics: Diagnostic[] };
    };
    if (msg.id !== undefined && !msg.method) {
      const p = pending.get(msg.id);
      pending.delete(msg.id);
      if (msg.error) p?.reject(new Error(msg.error.message));
      else p?.resolve(msg.result);
      return;
    }
    if (msg.method === "textDocument/publishDiagnostics" && msg.params) {
      const model = monaco.editor.getModel(monaco.Uri.parse(msg.params.uri));
      if (!model) return;
      monaco.editor.setModelMarkers(
        model,
        "tofu-ls",
        msg.params.diagnostics.map((d) => ({
          ...toRange(monaco, d.range).toJSON(),
          severity: d.severity === 1 ? monaco.MarkerSeverity.Error : d.severity === 2 ? monaco.MarkerSeverity.Warning : monaco.MarkerSeverity.Info,
          message: d.message,
          source: d.source,
        })) as Monaco.editor.IMarkerData[],
      );
    }
  };
  ws.onclose = () => {
    for (const p of pending.values()) p.reject(new Error("the language server closed"));
    pending.clear();
    if (!disposed) onClosed();
  };

  const providers = [
    monaco.languages.registerCompletionItemProvider("hcl", {
      triggerCharacters: [".", '"'],
      provideCompletionItems: async (model, position) => {
        const result = (await request("textDocument/completion", {
          textDocument: { uri: model.uri.toString() },
          position: { line: position.lineNumber - 1, character: position.column - 1 },
        }).catch(() => null)) as { items: CompletionItem[] } | CompletionItem[] | null;
        const items = Array.isArray(result) ? result : (result?.items ?? []);
        const word = model.getWordUntilPosition(position);
        const fallback = new monaco.Range(position.lineNumber, word.startColumn, position.lineNumber, word.endColumn);
        return {
          suggestions: items.map((it) => ({
            label: it.label,
            kind: it.kind === 7 ? monaco.languages.CompletionItemKind.Struct : monaco.languages.CompletionItemKind.Property,
            detail: it.detail,
            documentation: it.documentation ? { value: text(it.documentation) } : undefined,
            insertText: it.textEdit?.newText ?? it.insertText ?? it.label,
            range: it.textEdit ? toRange(monaco, it.textEdit.range) : fallback,
          })),
        };
      },
    }),
    monaco.languages.registerHoverProvider("hcl", {
      provideHover: async (model, position) => {
        const h = (await request("textDocument/hover", {
          textDocument: { uri: model.uri.toString() },
          position: { line: position.lineNumber - 1, character: position.column - 1 },
        }).catch(() => null)) as Hover | null;
        if (!h) return null;
        const parts = Array.isArray(h.contents) ? h.contents : [h.contents];
        return { contents: parts.map((p) => ({ value: text(p) })), range: h.range && toRange(monaco, h.range) };
      },
    }),
    monaco.languages.registerDocumentFormattingEditProvider("hcl", {
      provideDocumentFormattingEdits: async (model, options) => {
        const edits = (await request("textDocument/formatting", {
          textDocument: { uri: model.uri.toString() },
          options: { tabSize: options.tabSize, insertSpaces: options.insertSpaces },
        }).catch(() => null)) as TextEdit[] | null;
        return (edits ?? []).map((e) => ({ range: toRange(monaco, e.range), text: e.newText }));
      },
    }),
  ];

  return {
    open: (model) => {
      versions.set(model.uri.toString(), 1);
      notify("textDocument/didOpen", {
        textDocument: { uri: model.uri.toString(), languageId: "opentofu", version: 1, text: model.getValue() },
      });
    },
    change: (model) => {
      const uri = model.uri.toString();
      const version = (versions.get(uri) ?? 1) + 1;
      versions.set(uri, version);
      notify("textDocument/didChange", { textDocument: { uri, version }, contentChanges: [{ text: model.getValue() }] });
    },
    close: (model) => {
      versions.delete(model.uri.toString());
      notify("textDocument/didClose", { textDocument: { uri: model.uri.toString() } });
    },
    dispose: () => {
      disposed = true;
      for (const p of providers) p.dispose();
      ws.close();
    },
  };
}
