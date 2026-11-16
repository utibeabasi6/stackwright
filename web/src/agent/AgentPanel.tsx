import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Action, Submit, useAction } from "../Action";
import { Link } from "@tanstack/react-router";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { agentApi, message, projectApi } from "../api";
import { AgentSessionStatus, type AgentSession, type AgentStep } from "../gen/stackwright/v1/agent_pb";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { useRole, useShell } from "../me";
import { Select } from "../Select";
import { useLoad } from "../useLoad";
import { useConfirm } from "../Confirm";

type Props = {
  org: string;
  project: string;
  workspace: string;
  // ask is text from "Ask agent to fix", to send once the sessions load.
  ask?: string;
  onAsked: () => void;
};

type Tone = "busy" | "good" | "attention" | "muted" | "bad";

const status: Record<AgentSessionStatus, { text: string; tone: Tone }> = {
  [AgentSessionStatus.UNSPECIFIED]: { text: "", tone: "muted" },
  [AgentSessionStatus.STARTING]: { text: "Starting", tone: "busy" },
  [AgentSessionStatus.READY]: { text: "Ready", tone: "good" },
  [AgentSessionStatus.WORKING]: { text: "Working", tone: "busy" },
  [AgentSessionStatus.WAITING_FOR_ANSWER]: { text: "Waits for your answer", tone: "attention" },
  [AgentSessionStatus.WAITING_FOR_APPROVAL]: { text: "Waits for your decision", tone: "attention" },
  [AgentSessionStatus.ASLEEP]: { text: "Asleep", tone: "muted" },
  [AgentSessionStatus.CLOSED]: { text: "Closed", tone: "muted" },
  [AgentSessionStatus.FAILED]: { text: "Failed", tone: "bad" },
};

const busy = (s?: AgentSession) => s?.status === AgentSessionStatus.WORKING || s?.status === AgentSessionStatus.STARTING;

type Payload = Record<string, unknown>;
const parse = (s: string): Payload => {
  try {
    return JSON.parse(s);
  } catch {
    return {};
  }
};
const text = (v: unknown) => (typeof v === "string" ? v : "");

// canTake lists the statuses in which a session takes a new message.
const canTake = [AgentSessionStatus.READY, AgentSessionStatus.ASLEEP, AgentSessionStatus.WAITING_FOR_ANSWER];

// AskAgent sends an error to the agent. It shows only to writers, on
// projects that the agent can work on.
type AskProps = { org: string; project: string; workspace: string; prompt: string; label?: string };

export function AskAgent({ org, project, workspace, prompt, label = "Ask agent to fix" }: AskProps) {
  const { projects, askAgent } = useShell();
  const writer = useRole(org) >= Role.WRITER;
  if (!writer || !projects?.find((p) => p.slug === project)?.linkedRepo) return null;
  return (
    <button type="button" className="link ask-agent" onClick={() => askAgent({ org, project, workspace }, prompt)}>
      {label}
    </button>
  );
}

// AgentPanel is a chat with the agent about one workspace: its sessions, the
// open one's conversation, and a composer that starts or continues one.
export function AgentPanel({ org, project, workspace, ask, onAsked }: Props) {
  const writer = useRole(org) >= Role.WRITER;
  const settings = useLoad(`agent-settings:${org}`, () => agentApi.getAgentSettings({ org }).then((r) => r.settings));
  const ws = useLoad(`${org}/${project}/${workspace}`, () => projectApi.getWorkspace({ org, project, workspace }));
  const sessions = useLoad(`agent-sessions:${org}/${project}/${workspace}`, () =>
    agentApi.listSessions({ org, project, workspace }).then((r) => r.sessions),
  );
  const [chosen, setChosen] = useState<string>();
  const [error, setError] = useState<string>();
  const current = chosen ?? sessions.data?.[0]?.id ?? "new";

  const start = async (msg: string) => {
    setError(undefined);
    try {
      const r = await agentApi.startSession({ org, project, workspace, message: msg });
      sessions.reload();
      setChosen(r.session?.id);
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  const list = sessions.data;
  useEffect(() => {
    if (!ask || !list || !writer) return;
    onAsked();
    const open = list.find((s) => canTake.includes(s.status));
    if (!open) {
      void start(ask);
      return;
    }
    setChosen(open.id);
    agentApi.sendMessage({ org, id: open.id, message: ask }).catch(() => start(ask));
    // start changes on every render; the effect runs once for each ask.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ask, list, writer]);

  if (settings.data && !settings.data.hasKey) {
    return (
      <Notice>
        The agent needs an OpenRouter key. An admin adds it in{" "}
        <Link to="/$org/settings/agent" params={{ org }}>
          Settings › Agent
        </Link>
        .
      </Notice>
    );
  }
  if (ws.data && !ws.data.project?.linkedRepo) {
    return (
      <Notice>
        The agent works on projects in a repository that a linked GitHub installation covers.{" "}
        <Link to="/$org/settings/github" params={{ org }}>
          Link a repository
        </Link>
        .
      </Notice>
    );
  }

  const options = [
    ...(sessions.data ?? []).map((s) => ({ value: s.id, label: s.title })),
    ...(current === "new" ? [{ value: "new", label: "New session" }] : []),
  ];
  return (
    <div className="agent">
      <div className="agent-head">
        {options.length > 0 && (
          <Select className="agent-switch" aria-label="Session" value={current} onChange={setChosen} options={options} />
        )}
        {writer && current !== "new" && (
          <button type="button" onClick={() => setChosen("new")}>
            + New
          </button>
        )}
      </div>
      {current === "new" ? (
        <>
          <div className="agent-scroll agent-empty">
            <p className="agent-hello">What should the agent change?</p>
            <p className="hint">
              It works in a sandbox on its own branch of {ws.data?.project?.linkedRepo || "the repository"}. Nothing reaches GitHub until a
              person commits it.
            </p>
            {error && <p className="error">{error}</p>}
          </div>
          {writer && <Composer placeholder="Ask for a change" onSend={start} />}
        </>
      ) : (
        <Session key={current} org={org} id={current} writer={writer} onChange={sessions.reload} />
      )}
    </div>
  );
}

function Notice({ children }: { children: ReactNode }) {
  return <p className="agent-notice muted">{children}</p>;
}

function useSession(org: string, id: string) {
  const [session, setSession] = useState<AgentSession>();
  const [steps, setSteps] = useState<AgentStep[]>([]);
  useEffect(() => {
    const abort = new AbortController();
    let cursor = 0n;
    let delay = 1000;
    (async () => {
      while (!abort.signal.aborted) {
        try {
          for await (const r of agentApi.watchSession({ org, id, afterSeq: cursor }, { signal: abort.signal })) {
            if (r.session) setSession(r.session);
            if (r.steps.length > 0) {
              const fresh = r.steps.filter((s) => s.seq > cursor);
              cursor = r.steps[r.steps.length - 1].seq;
              setSteps((prev) => [...prev, ...fresh]);
            }
            delay = 1000;
          }
        } catch (err) {
          if (abort.signal.aborted || (err instanceof ConnectError && err.code === Code.NotFound)) return;
          await new Promise((r) => setTimeout(r, delay));
          delay = Math.min(delay * 2, 30000);
          continue;
        }
        await new Promise((r) => setTimeout(r, 1000));
      }
    })();
    return () => abort.abort();
  }, [org, id]);
  return { session, steps };
}

function Session({ org, id, writer, onChange }: { org: string; id: string; writer: boolean; onChange: () => void }) {
  const confirm = useConfirm();
  const { session, steps } = useSession(org, id);
  const [error, setError] = useState<string>();
  const scroll = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);

  useEffect(() => {
    const el = scroll.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [steps.length]);

  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    try {
      await fn();
      onChange();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const results = new Map(steps.filter((s) => s.kind === "tool_result").map((s) => [text(parse(s.payload).id), parse(s.payload)]));
  const decided = new Set(steps.filter((s) => s.kind === "decision").map((s) => Number(parse(s.payload).proposal)));
  const waiting = session?.status;
  const st = session ? status[session.status] : undefined;

  return (
    <>
      {session && (
        <div className="agent-meta">
          {st && (
            <span className={`agent-status ${st.tone}`}>
              <span className="dot" aria-hidden="true" />
              {st.text}
            </span>
          )}
          <span className="mono" title="The session's branch">
            {session.branch}
          </span>
          {session.prNumber > 0 && (
            <a href={session.prUrl} target="_blank" rel="noreferrer noopener">
              Pull request #{session.prNumber}
            </a>
          )}
          {session.spent > 0 && <span>${session.spent.toFixed(2)}</span>}
          {writer && !busy(session) && (
            <button
              type="button"
              className="link"
              onClick={async () => {
                if (
                  session.dirty &&
                  !(await confirm({ message: "Close this session? Its uncommitted changes are lost.", action: "Close session", danger: true }))
                ) {
                  return;
                }
                void act(() => agentApi.closeSession({ org, id, discard: true }));
              }}
            >
              Close
            </button>
          )}
        </div>
      )}
      <div
        className="agent-scroll"
        ref={scroll}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
      >
        <ol className="agent-steps" aria-label="Conversation" aria-live="polite">
          {steps.map((s) => (
            <Step
              key={String(s.seq)}
              step={s}
              results={results}
              canDecide={writer && waiting === AgentSessionStatus.WAITING_FOR_APPROVAL && !decided.has(Number(s.seq))}
              decide={(approve) => act(() => agentApi.decideProposal({ org, id, proposal: s.seq, approve }))}
            />
          ))}
          {busy(session) && (
            <li className="agent-working" aria-label="The agent is working">
              <span className="dot" />
              <span className="dot" />
              <span className="dot" />
            </li>
          )}
        </ol>
      </div>
      {error && <p className="error agent-error">{error}</p>}
      {writer && session && (
        <Composer
          placeholder={
            waiting === AgentSessionStatus.WAITING_FOR_ANSWER
              ? "Answer the question"
              : waiting === AgentSessionStatus.WAITING_FOR_APPROVAL
                ? "Decide on the proposal first"
                : "Ask for a change"
          }
          disabled={waiting === AgentSessionStatus.WAITING_FOR_APPROVAL}
          working={busy(session)}
          onStop={() => act(() => agentApi.stopTurn({ org, id }))}
          onSend={(msg) => act(() => agentApi.sendMessage({ org, id, message: msg }))}
        />
      )}
    </>
  );
}

type ComposerProps = {
  placeholder: string;
  disabled?: boolean;
  working?: boolean;
  onSend: (msg: string) => Promise<boolean>;
  onStop?: () => Promise<boolean>;
};

function Composer({ placeholder, disabled, working, onSend, onStop }: ComposerProps) {
  const [draft, setDraft] = useState("");
  const sending = useAction();
  const send = async () => {
    if (sending.state === "busy") return;
    await sending.run(async () => {
      const msg = draft.trim();
      if (!msg || working || disabled) return false;
      const sent = await onSend(msg);
      if (sent) setDraft("");
      return sent;
    });
  };
  const keyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      void send();
    }
  };
  return (
    <form
      className="agent-composer"
      aria-label="Message the agent"
      onSubmit={(e) => {
        e.preventDefault();
        void send();
      }}
    >
      <div className="box">
        <textarea
          aria-label="Message"
          rows={2}
          value={draft}
          placeholder={placeholder}
          disabled={disabled}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={keyDown}
        />
        {working ? (
          <Action className="danger" run={() => onStop?.()} busy="Stopping…" done="Stopped">
            ■ Stop
          </Action>
        ) : (
          <Submit action={sending} className="primary" disabled={!draft.trim() || disabled} busy="Sending…" done="Sent">
            Send
          </Submit>
        )}
      </div>
      <p className="hint">Enter sends · Shift+Enter starts a new line</p>
    </form>
  );
}

// toolLine names a tool call in a few words, as a person would say it.
function toolLine(name: string, args: Payload): string {
  const path = text(args.path);
  switch (name) {
    case "list_files":
      return path ? `Listed files in ${path}` : "Listed the files";
    case "read_file":
      return `Read ${path}`;
    case "search":
      return `Searched for “${text(args.pattern)}”${path ? ` in ${path}` : ""}`;
    case "edit_file":
      return `Edited ${path}`;
    case "write_file":
      return `Wrote ${path}`;
    case "fmt":
      return "Formatted the code";
    case "validate":
      return "Ran validate";
    case "propose_commit":
      return "Proposed a commit";
    case "run_plan":
      return "Ran a plan";
    case "plan_details":
      return `Read the plan for ${text(args.address)}`;
    case "state_lookup":
      return `Looked up ${text(args.address)} in the state`;
    case "provider_schema":
      return `Read the schema of ${text(args.kind) === "data" ? "data source " : ""}${text(args.type)}`;
    case "module_docs":
      return text(args.name) ? `Read the inputs of module ${text(args.name)}` : "Listed the module calls";
    case "propose_pull_request":
      return "Proposed a pull request";
  }
  return name;
}

type StepProps = {
  step: AgentStep;
  results: Map<string, Payload>;
  canDecide: boolean;
  decide: (approve: boolean) => Promise<boolean>;
};

function Step({ step, results, canDecide, decide }: StepProps) {
  const p = parse(step.payload);
  switch (step.kind) {
    case "user":
      return (
        <li className="msg you">
          <span className="who">You</span>
          <div className="body">{text(p.text)}</div>
        </li>
      );
    case "assistant":
      return text(p.text) ? (
        <li className="msg agent-msg">
          <span className="who">Agent</span>
          <div className="body prose">
            <Prose text={text(p.text)} />
          </div>
        </li>
      ) : null;
    case "tool_call": {
      const args = parse(text(p.arguments));
      if (p.name === "ask_user") {
        return (
          <li className="msg question">
            <span className="who">Agent asks</span>
            <div className="body prose">
              <Prose text={text(args.question)} />
            </div>
          </li>
        );
      }
      const r = results.get(text(p.id));
      const state = !r ? "running" : r.error === true ? "failed" : "done";
      const mark = { running: "…", failed: "!", done: "✓" }[state];
      return (
        <li className={`tool ${state}`}>
          <details>
            <summary>
              <span className="mark" aria-hidden="true">
                {mark}
              </span>
              <span className="line">{toolLine(text(p.name), args)}</span>
              <span className="sr-only">{state === "failed" ? ", failed" : state === "running" ? ", running" : ""}</span>
            </summary>
            {r && <pre>{text(r.output) || "(no output)"}</pre>}
          </details>
        </li>
      );
    }
    case "tool_result":
      return p.answer === true ? (
        <li className="msg you">
          <span className="who">You</span>
          <div className="body">{text(p.output)}</div>
        </li>
      ) : null;
    case "proposal":
      return p.type === "pull_request" ? (
        <PullRequest title={text(p.title)} body={text(p.body)} canDecide={canDecide} decide={decide} />
      ) : (
        <Proposal message={text(p.message)} diff={text(p.diff)} canDecide={canDecide} decide={decide} />
      );
    case "decision":
    case "notice":
    case "edit":
      return <li className="note">{text(p.text)}</li>;
    case "error":
      return <li className="note bad">! {text(p.text)}</li>;
  }
  return null;
}

// Prose renders a reply's markdown. Images are dropped, so a reply cannot
// make the browser fetch a URL; links open in a new tab.
function Prose({ text: t }: { text: string }) {
  return (
    <Markdown
      remarkPlugins={[remarkGfm]}
      disallowedElements={["img"]}
      unwrapDisallowed
      components={{
        a: ({ href, children }) => (
          <a href={href} target="_blank" rel="noreferrer noopener">
            {children}
          </a>
        ),
      }}
    >
      {t}
    </Markdown>
  );
}

function Proposal({ message: msg, diff, canDecide, decide }: { message: string; diff: string; canDecide: boolean; decide: (approve: boolean) => Promise<boolean> }) {
  const [shown, setShown] = useState(false);
  // git diff --stat lines look like " main.tf | 3 +++".
  const files = diff
    .split("\n")
    .map((l) => l.match(/^\s*(\S.*?)\s+\|\s+(\d+)\s*([+-]*)/))
    .filter((m): m is RegExpMatchArray => m !== null)
    .map((m) => ({ name: m[1], adds: (m[3].match(/\+/g) ?? []).length, dels: (m[3].match(/-/g) ?? []).length }));
  const patch = diff.slice(diff.indexOf("diff --git") >= 0 ? diff.indexOf("diff --git") : 0);
  return (
    <li className="proposal" aria-label="Proposed commit">
      <h3>Commit these changes?</h3>
      <p className="mono">{msg}</p>
      <ul className="files">
        {files.map((f) => (
          <li key={f.name}>
            <span className="mono">{f.name}</span>
            {f.adds > 0 && <span className="create"> +{f.adds}</span>}
            {f.dels > 0 && <span className="destroy"> −{f.dels}</span>}
          </li>
        ))}
      </ul>
      <button type="button" className="link" aria-expanded={shown} onClick={() => setShown(!shown)}>
        {shown ? "Hide the diff" : "Show the diff"}
      </button>
      {shown && (
        <pre className="diff">
          {patch.split("\n").map((l, i) => (
            <span key={i} className={l.startsWith("+") && !l.startsWith("+++") ? "add" : l.startsWith("-") && !l.startsWith("---") ? "del" : undefined}>
              {l + "\n"}
            </span>
          ))}
        </pre>
      )}
      {canDecide && (
        <div className="row">
          <Action run={() => decide(false)} busy="Discarding…" done="Declined">
            Discard
          </Action>
          <Action className="primary" run={() => decide(true)} busy="Approving…" done="Approved">
            Commit and push
          </Action>
        </div>
      )}
    </li>
  );
}

type PullRequestProps = { title: string; body: string; canDecide: boolean; decide: (approve: boolean) => Promise<boolean> };

function PullRequest({ title, body, canDecide, decide }: PullRequestProps) {
  return (
    <li className="proposal" aria-label="Proposed pull request">
      <h3>Open this pull request?</h3>
      <p>
        <strong>{title}</strong>
      </p>
      <div className="prose">
        <Prose text={body} />
      </div>
      <p className="hint">It opens from the session's branch, ready for review, and names you as the person who approved it.</p>
      {canDecide && (
        <div className="row">
          <Action run={() => decide(false)} busy="Declining…" done="Declined">
            Decline
          </Action>
          <Action className="primary" run={() => decide(true)} busy="Approving…" done="Approved">
            Open pull request
          </Action>
        </div>
      )}
    </li>
  );
}
