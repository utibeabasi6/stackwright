import { Action } from "./Action";

export function Copy({ label, text }: { label: string; text: string }) {
  return (
    <div className="code-block">
      <pre aria-label={label}>{text}</pre>
      <Action run={() => navigator.clipboard.writeText(text)} busy="Copying…" done="Copied">
        Copy
      </Action>
    </div>
  );
}
