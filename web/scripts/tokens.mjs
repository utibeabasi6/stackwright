import { readFileSync } from "node:fs";

const t = JSON.parse(readFileSync(new URL("../../docs/design/tokens.json", import.meta.url)));
const vars = (pairs) => pairs.map(([k, v]) => `  --${k}: ${v};`).join("\n");

const light = t.color.tokens.map((c) => [c.name, c.value.light]);
const dark = t.color.tokens.map((c) => [c.name, c.value.dark]);
const fonts = Object.entries(t.type.families).map(([k, v]) => [`font-${k}`, v]);
const styles = t.type.groups.flatMap((g) =>
  g.styles.map((s) => [`type-${s.name}`, `${s.fontWeight} ${s.fontSize}/${s.lineHeight} var(--font-${g.family})`]),
);
const lengths = ["spacing", "radius", "size"].flatMap((f) => t[f].tokens.map((x) => [x.name, x.value]));

process.stdout.write(`/* Generated from docs/design/tokens.json by scripts/tokens.mjs. */
:root {
  color-scheme: light dark;
${vars([...light, ...fonts, ...styles, ...lengths])}
}

@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
${vars(dark).replaceAll("\n  ", "\n    ").replace(/^ {2}/, "    ")}
  }
}

:root[data-theme="light"] {
  color-scheme: light;
}

:root[data-theme="dark"] {
  color-scheme: dark;
${vars(dark)}
}
`);
