import { useState, type FormEvent } from "react";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Submit, useAction } from "../Action";
import { securityApi } from "../api";
import { AskAgent } from "../agent/AgentPanel";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { RunStatus } from "../gen/stackwright/v1/runs_pb";
import { Severity, type Finding } from "../gen/stackwright/v1/security_pb";
import { useShell } from "../me";
import { decidesRisk } from "../roles";
import { Select } from "../Select";
import { useLoad } from "../useLoad";

const severities: Record<Severity, { text: string; tone: string }> = {
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
  const { me } = useShell();
  const decides = decidesRisk(me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED);
  const [accepting, setAccepting] = useState<string>();
  return (
    <div className="table-box">
      <table className="table">
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
              <td className="stack">
                <span>
                  <strong>{f.title}</strong> <span className="mono muted">{f.checkId}</span>
                </span>
                <span className="mono">{f.resource}</span>
                <span className="muted">{f.message}</span>
                {accepting === f.id && (
                  <AcceptForm
                    org={org}
                    finding={f}
                    done={() => {
                      setAccepting(undefined);
                      reload();
                    }}
                  />
                )}
              </td>
              <td className="stack">
                <FindingStatus finding={f} />
                {!f.fixed && !f.acceptance && f.id && decides && accepting !== f.id && (
                  <button type="button" className="link" onClick={() => setAccepting(f.id)}>
                    Accept risk
                  </button>
                )}
                {!f.fixed && (
                  <AskAgent
                    org={org}
                    project={f.project}
                    prompt={`The security review found ${f.checkId} (${f.title}) on ${f.resource}: ${f.message}\nFix the code so the finding goes away.`}
                  />
                )}
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
  return <span>Open</span>;
}

function AcceptForm({ org, finding, done }: { org: string; finding: Finding; done: () => void }) {
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
    <form className="stack card" onSubmit={submit}>
      <label className="field">
        Why is this risk acceptable?
        <textarea name="reason" required maxLength={2000} rows={2} />
      </label>
      <div className="row">
        <Select aria-label="Accept for" value={days} onChange={setDays} options={lifetimes} />
        <Submit action={action} busy="Accepting…" done="Accepted">
          Accept risk
        </Submit>
      </div>
      <span className="hint">The finding blocks applies again when the acceptance expires.</span>
    </form>
  );
}

export function WorkspaceSecurity({ org, project, workspace }: { org: string; project: string; workspace: string }) {
  const [fixed, setFixed] = useState(false);
  const list = useLoad(`findings:${org}/${project}/${workspace}:${fixed}`, () =>
    securityApi.listFindings({ org, project, workspace, includeFixed: fixed }).then((r) => r.findings),
  );
  return (
    <div className="stack">
      <p className="muted">
        Each plan of this workspace's own code is checked for security misconfigurations. A High or Critical finding on a resource that a
        plan creates or changes blocks its apply, until the code is fixed or someone accepts the risk. A plan runs every night at 02:00
        UTC, so findings follow new checks.
      </p>
      <label className="check">
        <input type="checkbox" checked={fixed} onChange={(e) => setFixed(e.target.checked)} /> Show fixed findings
      </label>
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
