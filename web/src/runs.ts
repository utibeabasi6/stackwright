import { RunStatus, type Run, type RunEvent } from "./gen/stackwright/v1/runs_pb";

type Tone = "ok" | "warn" | "error" | "muted";

const statuses: Record<RunStatus, { text: string; tone: Tone }> = {
  [RunStatus.UNSPECIFIED]: { text: "Unknown", tone: "muted" },
  [RunStatus.QUEUED]: { text: "Queued", tone: "muted" },
  [RunStatus.PLANNING]: { text: "Planning", tone: "muted" },
  [RunStatus.NEEDS_APPROVAL]: { text: "~ Needs approval", tone: "warn" },
  [RunStatus.APPLYING]: { text: "Applying", tone: "muted" },
  [RunStatus.PLANNED]: { text: "✓ Planned", tone: "ok" },
  [RunStatus.APPLIED]: { text: "✓ Applied", tone: "ok" },
  [RunStatus.FAILED]: { text: "! Failed", tone: "error" },
  [RunStatus.REJECTED]: { text: "Rejected", tone: "muted" },
  [RunStatus.CANCELED]: { text: "Canceled", tone: "muted" },
};

export const statusOf = (s: RunStatus) => statuses[s];

export const finished = (s: RunStatus) =>
  [RunStatus.PLANNED, RunStatus.APPLIED, RunStatus.FAILED, RunStatus.REJECTED, RunStatus.CANCELED].includes(s);

export const active = (s: RunStatus) => [RunStatus.PLANNING, RunStatus.NEEDS_APPROVAL, RunStatus.APPLYING].includes(s);

export function runTitle(r: Run) {
  return `${r.planOnly ? "Plan" : "Run"} #${r.number}`;
}

export function changes(r: Pick<Run, "adds" | "changes" | "destroys">) {
  return [
    { text: `+${r.adds}`, tone: r.adds ? "ok" : "muted" },
    { text: `~${r.changes}`, tone: r.changes ? "warn" : "muted" },
    { text: `−${r.destroys}`, tone: r.destroys ? "error" : "muted" },
  ];
}

type Line = { text: string; tone?: Tone };

const engineNames: Record<string, string> = { ENGINE_OPENTOFU: "OpenTofu", ENGINE_TERRAFORM: "Terraform" };

export function describeEvent(e: RunEvent): Line {
  let p: Record<string, unknown> = {};
  try {
    p = JSON.parse(e.payload);
  } catch {
    // An event's payload is JSON; show the type when it is not.
  }
  const by = e.actor ? ` by ${e.actor}` : "";
  switch (e.type) {
    case "run.queued":
      return { text: `Queued${by}, from ${p.source}${p.plan_only ? ", plan only" : ""}` };
    case "run.planning":
      return { text: "The workspace is free; waiting for a build server" };
    case "run.waiting": {
      const labels = Object.entries((p.labels as Record<string, string>) ?? {}).map(([k, v]) => `${k}=${v}`);
      return {
        text: labels.length
          ? `Waits for a build server with ${labels.join(", ")} and a free slot`
          : "Waits for a build server with a free slot",
        tone: "warn",
      };
    }
    case "run.dispatched":
      return { text: `Sent the ${p.operation} to build server ${p.build_server}` };
    case "run.summary":
      return p.has_changes
        ? { text: `Plan: +${p.adds} ~${p.changes} −${p.destroys}` }
        : { text: "Plan: no changes" };
    case "run.needs_approval":
      return { text: "Waits for approval", tone: "warn" };
    case "run.auto_approved":
      return { text: "Auto-apply is on, so the apply starts" };
    case "run.approved":
      return { text: `Approved${by}` };
    case "run.planned":
      return { text: "✓ Planned", tone: "ok" };
    case "run.applied":
      return { text: "✓ Applied", tone: "ok" };
    case "run.failed":
      return { text: `! Failed: ${p.error}`, tone: "error" };
    case "run.rejected":
      return { text: `Rejected${by}` };
    case "run.canceled":
      return { text: `Canceled${by}` };
    case "executor.started":
      return { text: `Sandbox started: ${engineNames[String(p.engine)] ?? p.engine} ${p.version}` };
    case "executor.engine_ready":
      return {
        text: p.cached
          ? "The engine binary from the build server's cache matches its signed checksum"
          : "The engine binary matches its signed checksum",
      };
    case "executor.failed":
      return { text: `! ${p.stage} failed: ${p.error}`, tone: "error" };
    case "executor.finished":
      return { text: p.succeeded ? "Sandbox finished" : "Sandbox finished with an error", tone: p.succeeded ? undefined : "error" };
  }
  const msg = typeof p["@message"] === "string" ? p["@message"] : e.type;
  return { text: msg, tone: p["@level"] === "error" ? "error" : undefined };
}
