import { PlanAction, type ActionCount, type PlanSummary } from "../gen/stackwright/v1/graph_pb";

type Info = { glyph: string; word: string; tone: "create" | "update" | "replace" | "destroy" | "muted" };

export const actionInfo: Record<PlanAction, Info> = {
  [PlanAction.UNSPECIFIED]: { glyph: "?", word: "unknown", tone: "muted" },
  [PlanAction.NO_OP]: { glyph: "", word: "no change", tone: "muted" },
  [PlanAction.READ]: { glyph: "<=", word: "read", tone: "muted" },
  [PlanAction.CREATE]: { glyph: "+", word: "create", tone: "create" },
  [PlanAction.UPDATE]: { glyph: "~", word: "update", tone: "update" },
  [PlanAction.REPLACE]: { glyph: "-/+", word: "replace", tone: "replace" },
  [PlanAction.REPLACE_CREATE_FIRST]: { glyph: "+/-", word: "replace, create first", tone: "replace" },
  [PlanAction.DELETE]: { glyph: "-", word: "destroy", tone: "destroy" },
};

export const countsText = (counts: ActionCount[]) =>
  counts
    .filter((c) => c.action !== PlanAction.NO_OP)
    .map((c) => `${actionInfo[c.action].glyph}${c.count}`)
    .join(" ");

export function summaryText(s: PlanSummary | undefined) {
  if (!s) return "";
  const parts = [`+${s.creates} ~${s.updates} -/+${s.replaces} −${s.deletes}`];
  if (s.reads) parts.push(`${s.reads} read`);
  if (s.unchanged) parts.push(`${s.unchanged} unchanged`);
  return parts.join(" · ");
}
