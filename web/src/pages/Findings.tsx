import { useState, type FormEvent } from "react";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { useNavigate } from "@tanstack/react-router";
import { Action, Submit, useAction } from "../Action";
import { runApi, securityApi } from "../api";
import { AskAgent } from "../agent/AgentPanel";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { RunStatus } from "../gen/stackwright/v1/runs_pb";
import { Severity, type Finding } from "../gen/stackwright/v1/security_pb";
import { useRole } from "../me";
import { decidesRisk } from "../roles";
import { Select } from "../Select";
import { useLoad } from "../useLoad";

export const severities: Record<Severity, { text: string; tone: string }> = {
  [Severity.UNSPECIFIED]: { text: "Unknown", tone: "muted" },
  [Severity.LOW]: { text: "Low", tone: "muted" },
  [Severity.MEDIUM]: { text: "Medium", tone: "warn" },
  [Severity.HIGH]: { text: "High", tone: "error" },
  [Severity.CRITICAL]: { text: "Critical", tone: "error" },
};

const lifetimes = [7, 30, 90].map((d) => ({ value: String(d), label: `${d} days` }));

type TableProps = {
  org: string;
  findings: Finding[];
  reload: () => void;
  // workspace columns show where each finding is, as on the inbox.
  where?: boolean;
};

export function FindingsTable({ org, findings, reload, where }: TableProps) {
  const role = useRole(org);
  const decides = decidesRisk(role);
  const writer = role >= Role.WRITER;
  const navigate = useNavigate();
  const [accepting, setAccepting] = useState<string>();
  return (
    <div className="table-box">
      <table className="table findings">
        <thead>
          <tr>
            <th>Severity</th>
            {where && <th>Workspace</th>}
            <th>Finding</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {findings.map((f) => (
            <tr key={f.id || `${f.checkId}/${f.resource}`}>
              <td>
                <span className={severities[f.severity].tone}>{severities[f.severity].text}</span>
              </td>
              {where && (
                <td className="mono">
                  {f.project} / {f.workspace}
                </td>
              )}
              <td>
                <div className="finding">
                  <span>
                    <strong>{f.title}</strong> <span className="mono muted">{f.checkId}</span>
                  </span>
                  <span className="mono">{f.resource}</span>
                  {f.unmatched && (
                    <span className="hint">
                      ~ Not matched to an address in the plan, so this finding never blocks an apply.
                    </span>
                  )}
                  <span className="muted">{f.message}</span>
                </div>
                {accepting === f.id && (
                  <AcceptForm
                    org={org}
                    finding={f}
                    done={() => {
                      setAccepting(undefined);
                      reload();
                    }}
                    cancel={() => setAccepting(undefined)}
                  />
                )}
              </td>
              <td className="finding-status">
                <FindingStatus finding={f} />
                <div className="finding-actions">
                  {f.checkId === "DRIFT" && !f.fixed && writer && (
                    <Action
                      className="link"
                      run={async () => {
                        const { run } = await runApi.createRun({ org, project: f.project, workspace: f.workspace, revert: true });
                        if (run) void navigate({ to: "/$org/projects/$project/$workspace/runs/$number", params: { org, project: f.project, workspace: f.workspace, number: String(run.number) } });
                      }}
                      busy="Starting…"
                      done="Started"
                    >
                      Revert drift
                    </Action>
                  )}
                  {f.checkId === "DRIFT" && !f.fixed && (
                    <AskAgent
                      org={org}
                      project={f.project}
                      workspace={f.workspace}
                      label="Reconcile with agent"
                      prompt={`${f.resource} changed outside Stackwright (${f.message}). Change the code so it matches what is deployed now.`}
                    />
                  )}
                  {f.checkId !== "DRIFT" && !f.fixed && !f.acceptance && f.id && decides && accepting !== f.id && (
                    <button type="button" className="link" onClick={() => setAccepting(f.id)}>
                      Accept risk
                    </button>
                  )}
                  {f.checkId !== "DRIFT" && !f.fixed && (
                    <AskAgent
                      org={org}
                      project={f.project}
                      workspace={f.workspace}
                      prompt={`The security review found ${f.checkId} (${f.title}) on ${f.resource}: ${f.message}\nFix the code so the finding goes away.`}
                    />
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function FindingStatus({ finding: f }: { finding: Finding }) {
  if (f.fixed) return <span className="ok">✓ Fixed</span>;
  if (f.acceptance) {
    return (
      <span className="muted" title={f.acceptance.reason}>
        Risk accepted by {f.acceptance.acceptedBy}
        {f.acceptance.expiresAt && `, until ${timestampDate(f.acceptance.expiresAt).toLocaleDateString()}`}
      </span>
    );
  }
  if (f.blocking) return <span className="error">! Blocks this apply</span>;
  if (!f.id) return <span className="muted">Not recorded: this plan is not of the workspace's own code</span>;
  return <span>{f.unmatched ? "Open, never blocks" : "Open"}</span>;
}

function AcceptForm({ org, finding, done, cancel }: { org: string; finding: Finding; done: () => void; cancel: () => void }) {
  const [days, setDays] = useState("30");
  const action = useAction();
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const reason = String(new FormData(e.currentTarget).get("reason"));
    void action.run(async () => {
      await securityApi.acceptRisk({
        org,
        id: finding.id,
        reason,
        expiresAt: timestampFromDate(new Date(Date.now() + Number(days) * 24 * 60 * 60 * 1000)),
      });
      done();
    });
  };
  return (
    <form className="stack card accept-form" onSubmit={submit} onKeyDown={(e) => e.key === "Escape" && cancel()}>
      <label className="field">
        Why is this risk acceptable?
        <textarea name="reason" required maxLength={2000} rows={2} />
      </label>
      <div className="row">
        <Select aria-label="Accept for" value={days} onChange={setDays} options={lifetimes} />
        <Submit action={action} busy="Accepting…" done="Accepted">
          Accept risk
        </Submit>
        <button type="button" onClick={cancel}>
          Cancel
        </button>
      </div>
      <span className="hint">The finding blocks applies again when the acceptance expires.</span>
    </form>
  );
}

export function WorkspaceSecurity({ org, project, workspace }: { org: string; project: string; workspace: string }) {
  const [fixed, setFixed] = useState(false);
  const writer = useRole(org) >= Role.WRITER;
  const navigate = useNavigate();
  const list = useLoad(`findings:${org}/${project}/${workspace}:${fixed}`, () =>
    securityApi.listFindings({ org, project, workspace, includeFixed: fixed }).then((r) => r.findings),
  );
  return (
    <div className="stack">
      <p className="muted">
        Each plan of this workspace's own code is checked for security misconfigurations. A High or Critical finding on a resource that a
        plan creates or changes blocks its apply, until the code is fixed or someone accepts the risk. A plan runs every night at 02:00
        UTC, so findings follow new checks. Every 6 hours, a drift check plans the code last applied with -refresh-only, and each
        resource changed outside Stackwright opens a drift finding, which never blocks.
      </p>
      <div className="row">
        <label className="check grow">
          <input type="checkbox" checked={fixed} onChange={(e) => setFixed(e.target.checked)} /> Show fixed findings
        </label>
        {writer && (
          <Action
            run={async () => {
              const { run } = await runApi.createRun({ org, project, workspace, checkDrift: true });
              if (run) void navigate({ to: "/$org/projects/$project/$workspace/runs/$number", params: { org, project, workspace, number: String(run.number) } });
            }}
            busy="Starting…"
            done="Started"
          >
            Check drift now
          </Action>
        )}
      </div>
      {list.error && <p className="error">{list.error}</p>}
      {list.data?.length === 0 && <p className="muted">No {fixed ? "" : "open "}findings.</p>}
      {!!list.data?.length && <FindingsTable org={org} findings={list.data} reload={list.reload} />}
    </div>
  );
}

type RunProps = { org: string; project: string; workspace: string; number: number; status: RunStatus };

export function RunFindings({ org, project, workspace, number, status }: RunProps) {
  const list = useLoad(`run-findings:${org}/${project}/${workspace}/${number}:${status}`, () =>
    securityApi.listRunFindings({ org, project, workspace, number }).then((r) => r.findings),
  );
  const findings = list.data ?? [];
  if (findings.length === 0) return null;
  const blocking = findings.filter((f) => f.blocking).length;
  return (
    <details className="run-findings" open={status === RunStatus.BLOCKED}>
      <summary>
        {findings.length} security {findings.length === 1 ? "finding" : "findings"}
        {blocking > 0 && <span className="error">, {blocking} {blocking === 1 ? "blocks" : "block"} the apply</span>}
      </summary>
      <FindingsTable org={org} findings={findings} reload={list.reload} />
    </details>
  );
}
