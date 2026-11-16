// Writes the licence and notice files of the web app's production
// packages, which its built bundle ships, from `pnpm licenses list --prod
// --json` on stdin.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

const byLicense = JSON.parse(readFileSync(0, "utf8"));
const out = [];
for (const p of Object.values(byLicense).flat().sort((a, b) => a.name.localeCompare(b.name))) {
  for (const dir of p.paths) {
    const files = readdirSync(dir).filter((f) => /^(licen[cs]e|notice|copying)/i.test(f));
    out.push(`${p.name}@${p.versions.join(", ")} (${p.license})\n${"=".repeat(72)}`);
    for (const f of files) {
      out.push(readFileSync(join(dir, f), "utf8").trim());
    }
    if (files.length === 0) {
      out.push(`No licence file ships with this package; its licence is ${p.license}.`);
    }
    out.push("");
  }
}
process.stdout.write(out.join("\n\n"));
