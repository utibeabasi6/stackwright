export function Copy({ label, text }: { label: string; text: string }) {
  return (
    <div className="code-block">
      <pre aria-label={label}>{text}</pre>
      <button type="button" onClick={() => navigator.clipboard.writeText(text)}>
        Copy
      </button>
    </div>
  );
}
