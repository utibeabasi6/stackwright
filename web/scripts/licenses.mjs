import { readFileSync } from "node:fs";

const allowed = new Set(["MIT", "ISC", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "0BSD", "Unlicense"]);
const ok = (expr) => expr.replace(/[()]/g, " ").split(/\s+(?:AND|OR)\s+|\s+/).filter(Boolean).every((id) => allowed.has(id));
const byLicense = JSON.parse(readFileSync(0, "utf8"));
const bad = Object.entries(byLicense)
  .filter(([license]) => !ok(license))
  .flatMap(([license, pkgs]) => pkgs.map((p) => `${p.name}@${p.versions.join(",")}: ${license}`));

if (bad.length) {
  console.error("licences not allowed:\n" + bad.join("\n"));
  process.exit(1);
}
console.log(`${Object.values(byLicense).flat().length} packages, all allowed`);
