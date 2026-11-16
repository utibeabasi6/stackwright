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
  [RunStatus.BLOCKED]: { text: "! Blocked", tone: "error" },
};

export const statusOf = (s: RunStatus) => statuses[s];

export const finished = (s: RunStatus) =>
  [RunStatus.PLANNED, RunStatus.APPLIED, RunStatus.FAILED, RunStatus.REJECTED, RunStatus.CANCELED].includes(s);

export function runTitle(r: Run) {
  return `${r.planOnly ? "Plan" : "Run"} #${r.number}`;
}

// origin says what started a run: a pull request, a push, or a person.
export function origin(r: Pick<Run, "trigger" | "prNumber" | "branch" | "commitSha" | "source">) {
  switch (r.trigger) {
    case "pull_request":
      return `pull request #${r.prNumber} · ${r.commitSha.slice(0, 7)}`;
    case "push":
      return `push to ${r.branch} · ${r.commitSha.slice(0, 7)}`;
    case "agent":
      return `the agent, on ${r.branch}`;
    case "schedule":
      return `the nightly review, from ${r.source}`;
    case "deployment":
      return `a deployment step, from ${r.source}`;
    case "drift":
      return `the drift check, of the code last applied`;
    case "cli":
      return `a speculative plan, from ${r.source}`;
  }
  return r.source;
}

export function changes(r: Pick<Run, "adds" | "changes" | "destroys">) {
  return [
    { text: `+${r.adds}`, tone: r.adds ? "ok" : "muted" },
    { text: `~${r.changes}`, tone: r.changes ? "warn" : "muted" },
    { text: `−${r.destroys}`, tone: r.destroys ? "error" : "muted" },
  ];
}

// line is an event's line of the timeline, which the server writes so the
// CLI prints the same (adr/0044).
export function line(e: RunEvent): { text: string; tone?: Tone; detail?: string; fix?: string } {
  return { text: e.text || e.type, tone: (e.tone || undefined) as Tone | undefined, detail: e.detail || undefined, fix: e.fix || undefined };
}
