import { readFileSync } from "node:fs";

const allowed = new Set(["MIT", "ISC", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "0BSD", "Unlicense"]);
// elkjs is "EPL-2.0 OR GPL-3.0-or-later"; Stackwright takes it under EPL-2.0 (adr/0027).
const exceptions = { elkjs: "EPL-2.0 OR GPL-3.0-or-later" };
const ok = (expr) => expr.replace(/[()]/g, " ").split(/\s+(?:AND|OR)\s+|\s+/).filter(Boolean).every((id) => allowed.has(id));
const byLicense = JSON.parse(readFileSync(0, "utf8"));
const bad = Object.entries(byLicense)
  .flatMap(([license, pkgs]) => pkgs.map((p) => ({ license, p })))
  .filter(({ license, p }) => !ok(license) && exceptions[p.name] !== license)
  .map(({ license, p }) => `${p.name}@${p.versions.join(",")}: ${license}`);

if (bad.length) {
  console.error("licences not allowed:\n" + bad.join("\n"));
  process.exit(1);
}
console.log(`${Object.values(byLicense).flat().length} packages, all allowed`);
