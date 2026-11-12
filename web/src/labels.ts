// Labels are typed as "region=eu, gpu=none".
export function parseLabels(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of text.split(",")) {
    const [k, ...v] = part.trim().split("=");
    if (k) out[k.trim()] = v.join("=").trim();
  }
  return out;
}

export function formatLabels(labels: Record<string, string>): string {
  return Object.entries(labels)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}=${v}`)
    .join(", ");
}
