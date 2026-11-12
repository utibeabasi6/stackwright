import { useEffect, useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { agentApi, message, projectApi } from "../api";
import { AgentSessionStatus, type AgentSession, type AgentStep } from "../gen/stackwright/v1/agent_pb";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useShell } from "../me";
import { Select } from "../Select";
import { useLoad } from "../useLoad";

type Props = { org: string; project: string; workspace: string };

const statusText: Record<AgentSessionStatus, string> = {
  [AgentSessionStatus.UNSPECIFIED]: "",
  [AgentSessionStatus.STARTING]: "Starting a sandbox and cloning the branch",
  [AgentSessionStatus.READY]: "Ready",
  [AgentSessionStatus.WORKING]: "Working",
  [AgentSessionStatus.WAITING_FOR_ANSWER]: "Waits for your answer",
  [AgentSessionStatus.WAITING_FOR_APPROVAL]: "Waits for a person to commit or discard",
  [AgentSessionStatus.ASLEEP]: "Asleep; a message starts a fresh sandbox",
  [AgentSessionStatus.CLOSED]: "Closed",
  [AgentSessionStatus.FAILED]: "Failed",
};

const busy = (s?: AgentSession) => s?.status === AgentSessionStatus.WORKING || s?.status === AgentSessionStatus.STARTING;

// AgentPanel holds a workspace's agent sessions and the open one.
export function AgentPanel({ org, project, workspace }: Props) {
  const { me } = useShell();
  const writer = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.WRITER;
  const settings = useLoad(`agent-settings:${org}`, () => agentApi.getAgentSettings({ org }).then((r) => r.settings));
  const ws = useLoad(`${org}/${project}/${workspace}`, () => projectApi.getWorkspace({ org, project, workspace }));
  const sessions = useLoad(`agent-sessions:${org}/${project}/${workspace}`, () =>
    agentApi.listSessions({ org, project, workspace }).then((r) => r.sessions),
  );
  const [open, setOpen] = useState<string>();
  const [error, setError] = useState<string>();
  const current = open ?? sessions.data?.[0]?.id;

  if (settings.data && !settings.data.hasKey) {
    return (
      <p className="muted">
        The agent needs an OpenRouter key. An admin adds it in{" "}
        <Link to="/$org/settings/agent" params={{ org }}>
          Settings › Agent
        </Link>
        .
      </p>
    );
  }
  if (ws.data && !ws.data.project?.linkedRepo) {
    return (
      <p className="muted">
        The agent works on projects in a repository that a linked GitHub installation covers.{" "}
        <Link to="/$org/settings/github" params={{ org }}>
          Link a repository
        </Link>
        .
      </p>
    );
  }

  const start = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    setError(undefined);
    try {
      const r = await agentApi.startSession({ org, project, workspace, message: String(new FormData(form).get("message")) });
      form.reset();
      sessions.reload();
      setOpen(r.session?.id);
    } catch (err) {
      setError(message(err));
    }
  };

  return (
    <div className="stack">
      {(sessions.data?.length ?? 0) > 0 && (
        <label className="field">
          <span>Session</span>
          <Select
            value={current ?? "new"}
            onChange={(v) => setOpen(v)}
            options={[
              ...(sessions.data ?? []).map((s) => ({ value: s.id, label: s.title })),
              ...(writer ? [{ value: "new", label: "New session…" }] : []),
            ]}
          />
        </label>
      )}
      {error && <p className="error">{error}</p>}
      {current && current !== "new" ? (
        <Session key={current} org={org} id={current} writer={writer} onChange={sessions.reload} />
      ) : (
        writer && (
          <form className="stack" onSubmit={start} aria-label="New agent session">
            <label className="field">
              <span>What should the agent change?</span>
              <textarea name="message" required rows={4} placeholder="Add a random_pet named db" />
            </label>
            <button type="submit" className="primary">
              Start
            </button>
            <p className="hint">The agent works on its own branch in a sandbox. Nothing reaches GitHub until a person commits it.</p>
          </form>
        )
      )}
    </div>
  );
}

function useSession(org: string, id: string) {
  const [session, setSession] = useState<AgentSession>();
  const [steps, setSteps] = useState<AgentStep[]>([]);
  useEffect(() => {
    const abort = new AbortController();
    let cursor = 0n;
    (async () => {
      while (!abort.signal.aborted) {
        try {
          for await (const r of agentApi.watchSession({ org, id, afterSeq: cursor }, { signal: abort.signal })) {
            if (r.session) setSession(r.session);
            if (r.steps.length > 0) {
              cursor = r.steps[r.steps.length - 1].seq;
              setSteps((prev) => [...prev, ...r.steps]);
            }
          }
        } catch {
          if (abort.signal.aborted) return;
        }
        await new Promise((r) => setTimeout(r, 1000));
      }
    })();
    return () => abort.abort();
  }, [org, id]);
  return { session, steps };
}

type Payload = Record<string, unknown>;
const parse = (s: string): Payload => {
  try {
    return JSON.parse(s);
  } catch {
    return {};
  }
};

function Session({ org, id, writer, onChange }: { org: string; id: string; writer: boolean; onChange: () => void }) {
  const { session, steps } = useSession(org, id);
  const [error, setError] = useState<string>();
  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    try {
      await fn();
      onChange();
    } catch (err) {
      setError(message(err));
    }
  };
  const send = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const text = String(new FormData(form).get("message"));
    void act(() => agentApi.sendMessage({ org, id, message: text })).then(() => form.reset());
  };
  const results = new Map(steps.filter((s) => s.kind === "tool_result").map((s) => [String(parse(s.payload).id), parse(s.payload)]));
  const decided = new Set(steps.filter((s) => s.kind === "decision").map((s) => Number(parse(s.payload).proposal)));
  const waitingProposal = session?.status === AgentSessionStatus.WAITING_FOR_APPROVAL;

  return (
    <div className="stack agent-session">
      {session && (
        <p className="hint">
          <span className="mono">{session.branch}</span> · {statusText[session.status]}
          {session.spent > 0 && ` · $${session.spent.toFixed(2)}`}
        </p>
      )}
      <ol className="agent-steps" aria-label="Conversation" aria-live="polite">
        {steps.map((s) => {
          const p = parse(s.payload);
          switch (s.kind) {
            case "user":
              return (
                <li key={String(s.seq)} className="from-user">
                  {String(p.text)}
                </li>
              );
            case "assistant":
              return p.text ? (
                <li key={String(s.seq)} className="from-agent">
                  {String(p.text)}
                </li>
              ) : null;
            case "tool_call": {
              const r = results.get(String(p.id));
              const args = parse(String(p.arguments ?? ""));
              const failed = r?.error === true;
              return (
                <li key={String(s.seq)} className={failed ? "tool destroy" : "tool"}>
                  <details>
                    <summary className="mono">
                      {failed ? "! " : r ? "✓ " : "… "}
                      {String(p.name)} {args.path ? String(args.path) : ""}
                      {p.name === "ask_user" ? `: ${String(args.question)}` : ""}
                    </summary>
                    {r && <pre className="mono">{String(r.output)}</pre>}
                  </details>
                </li>
              );
            }
            case "proposal":
              return (
                <li key={String(s.seq)} className="proposal card stack">
                  <h3>Commit these changes?</h3>
                  <p className="mono">{String(p.message)}</p>
                  <details>
                    <summary>Show diff</summary>
                    <pre className="mono diff">{String(p.diff)}</pre>
                  </details>
                  {writer && waitingProposal && !decided.has(Number(s.seq)) && (
                    <div className="row">
                      <button type="button" onClick={() => act(() => agentApi.decideProposal({ org, id, proposal: s.seq, approve: false }))}>
                        Discard
                      </button>
                      <button
                        type="button"
                        className="primary"
                        onClick={() => act(() => agentApi.decideProposal({ org, id, proposal: s.seq, approve: true }))}
                      >
                        Commit and push
                      </button>
                    </div>
                  )}
                </li>
              );
            case "decision":
            case "notice":
              return (
                <li key={String(s.seq)} className="notice muted">
                  {String(p.text)}
                </li>
              );
            case "error":
              return (
                <li key={String(s.seq)} className="error">
                  ! {String(p.text)}
                </li>
              );
          }
          return null;
        })}
      </ol>
      {error && <p className="error">{error}</p>}
      {writer && session && (
        <form className="stack" onSubmit={send} aria-label="Message the agent">
          <textarea
            name="message"
            required
            rows={3}
            aria-label={session.status === AgentSessionStatus.WAITING_FOR_ANSWER ? "Your answer" : "Message"}
            placeholder={session.status === AgentSessionStatus.WAITING_FOR_ANSWER ? "Answer the question" : "Ask for a change"}
            disabled={busy(session) || waitingProposal}
          />
          <div className="row">
            {busy(session) ? (
              <button type="button" className="danger" onClick={() => act(() => agentApi.stopTurn({ org, id }))}>
                Stop
              </button>
            ) : (
              <button type="submit" disabled={waitingProposal}>
                Send
              </button>
            )}
          </div>
        </form>
      )}
    </div>
  );
}
