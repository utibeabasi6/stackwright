// @vitest-environment node
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

test("tokens.css matches docs/design/tokens.json", () => {
  const fresh = execFileSync("node", ["scripts/tokens.mjs"], { encoding: "utf8" });
  expect(readFileSync("src/tokens.css", "utf8")).toBe(fresh);
});
