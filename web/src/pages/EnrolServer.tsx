import { useState, type FormEvent } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { buildServerApi, message } from "../api";
import { Copy } from "../Copy";
import { parseLabels } from "../labels";
import { useLoad } from "../useLoad";

type Method = "ssh" | "command";

export function EnrolServer() {
  const { org } = useParams({ strict: false }) as { org: string };
  const [method, setMethod] = useState<Method>("ssh");
  const tab = (m: Method, label: string) => (
    <button type="button" role="tab" aria-selected={method === m} onClick={() => setMethod(m)}>
      {label}
    </button>
  );
  return (
    <div className="stack">
      <div>
        <h1>Enrol a build server</h1>
        <p className="muted">A Linux machine with Docker. Stackwright starts sandboxes there for this org's jobs.</p>
      </div>
      <div role="tablist" aria-label="Enrol method" className="tabs">
        {tab("ssh", "Over SSH")}
        {tab("command", "Run a command on the server")}
      </div>
      <div className="row enrol">
        {method === "ssh" ? <OverSSH org={org} /> : <WithCommand org={org} />}
        <aside className="card stack prose" aria-labelledby="changes-h">
          <h2 id="changes-h">What changes on the server</h2>
          <ul>
            <li>
              <code>/usr/local/bin/stackwright-builder</code> and its systemd service, which runs as root to use Docker.
            </li>
            <li>The builder opens one outbound connection to this instance's builder port. The server needs no open port.</li>
            <li>Nothing else. With no Docker on the server, enrolment stops before it installs anything.</li>
          </ul>
          <p className="notice">Code from plans runs on this server, inside sandboxes. Use a machine that holds nothing else you care about.</p>
        </aside>
      </div>
    </div>
  );
}

function ServerFields() {
  return (
    <div className="row">
      <label className="field">
        <span>Name</span>
        <input name="name" required pattern="[a-z0-9][a-z0-9\-]{0,62}" placeholder="build-fsn-01" />
      </label>
      <label className="field">
        <span>Most sandboxes at once</span>
        <input name="slots" type="number" min={1} max={64} defaultValue={2} required />
      </label>
      <label className="field grow">
        <span>Labels</span>
        <input name="labels" placeholder="region=eu" />
      </label>
    </div>
  );
}

function OverSSH({ org }: { org: string }) {
  const key = useLoad(`ssh-key:${org}`, () => buildServerApi.getSSHKey({ org }));
  const [address, setAddress] = useState("");
  const [port, setPort] = useState(22);
  const [scan, setScan] = useState<{ hostKey: string; fingerprint: string; host: string }>();
  const [confirmed, setConfirmed] = useState(false);
  const [steps, setSteps] = useState<string[]>([]);
  const [done, setDone] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [user, host] = address.includes("@") ? address.split("@", 2) : ["root", address];

  const readKey = async () => {
    setError(undefined);
    setScan(undefined);
    setConfirmed(false);
    try {
      const r = await buildServerApi.scanHostKey({ org, host, port });
      setScan({ ...r, host });
    } catch (err) {
      setError(message(err));
    }
  };

  const install = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!scan) return;
    const f = new FormData(e.currentTarget);
    setError(undefined);
    setSteps([]);
    setDone(undefined);
    setBusy(true);
    try {
      for await (const r of buildServerApi.enrolOverSSH({
        org,
        name: String(f.get("name")),
        host,
        port,
        user,
        hostKey: scan.hostKey,
        slots: Number(f.get("slots")),
        labels: parseLabels(String(f.get("labels") ?? "")),
      })) {
        if (r.done) setDone(r.buildServer?.name);
        else setSteps((s) => [...s, r.step]);
      }
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="stack grow" onSubmit={install}>
      <div className="row">
        <label className="field grow">
          <span>SSH address</span>
          <input
            value={address}
            onChange={(e) => {
              setAddress(e.target.value.trim());
              setScan(undefined);
            }}
            required
            placeholder="root@203.0.113.12"
            className="mono"
          />
          <span className="hint">A user other than root needs passwordless sudo.</span>
        </label>
        <label className="field">
          <span>Port</span>
          <input type="number" min={1} max={65535} value={port} onChange={(e) => setPort(Number(e.target.value))} required />
        </label>
      </div>
      <ServerFields />

      <section className="stack" aria-labelledby="key-h">
        <h2 id="key-h">1. Let Stackwright sign in</h2>
        <p className="muted">
          Add this public key to <code>~/.ssh/authorized_keys</code> of that user on the server. Each org has its own key.
        </p>
        {key.error && <p className="error">{key.error}</p>}
        {key.data && <Copy label="The org's public SSH key" text={key.data.publicKey} />}
      </section>

      <section className="stack" aria-labelledby="host-h">
        <h2 id="host-h">2. Check the host key</h2>
        {!scan ? (
          <div className="row">
            <button type="button" onClick={readKey} disabled={!host}>
              Read the host key
            </button>
            <span className="hint">Stackwright connects to {host || "the server"} and shows the key it offers.</span>
          </div>
        ) : (
          <>
            <p className="muted">
              {scan.host} offers this key. Run <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> on the server and compare.
            </p>
            <pre className="mono">{`${scan.hostKey.split(" ")[0]} ${scan.fingerprint}`}</pre>
            <label className="check">
              <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} /> The fingerprint matches
            </label>
          </>
        )}
      </section>

      {error && <p className="error">{error}</p>}
      {(steps.length > 0 || done) && (
        <section className="card stack" aria-live="polite" aria-labelledby="progress-h">
          <h2 id="progress-h">{done ? `${done} · enrolled` : "Installing"}</h2>
          <ol className="steps mono">
            {steps.map((s, i) => (
              <li key={s}>
                {i < steps.length - 1 || done ? <span className="ok">✓</span> : error ? <span className="error">!</span> : "·"} {s}
              </li>
            ))}
          </ol>
          {done && (
            <Link to="/$org/build-servers" params={{ org }}>
              See it in Build servers
            </Link>
          )}
        </section>
      )}
      {!done && (
        <div className="row">
          <button type="submit" className="primary" disabled={!scan || !confirmed || busy}>
            Connect and install
          </button>
          <Link className="button" to="/$org/build-servers" params={{ org }}>
            Cancel
          </Link>
        </div>
      )}
    </form>
  );
}

function WithCommand({ org }: { org: string }) {
  const [result, setResult] = useState<{ command: string; expires: string }>();
  const [revealed, setRevealed] = useState(false);
  const [error, setError] = useState<string>();

  const make = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setError(undefined);
    try {
      const r = await buildServerApi.createJoinCommand({
        org,
        name: String(f.get("name")),
        slots: Number(f.get("slots")),
        labels: parseLabels(String(f.get("labels") ?? "")),
      });
      setResult({ command: r.command, expires: r.expiresAt ? timestampDate(r.expiresAt).toLocaleTimeString() : "" });
      setRevealed(false);
    } catch (err) {
      setError(message(err));
    }
  };

  const shown = result && (revealed ? result.command : result.command.replace(/(swe_.{4})[A-Za-z0-9_-]+/, "$1…"));
  return (
    <form className="stack grow" onSubmit={make}>
      <p className="muted">For a server that Stackwright cannot reach over SSH. You run one script as root on the server.</p>
      <ServerFields />
      {error && <p className="error">{error}</p>}
      {result && shown ? (
        <section className="stack" aria-labelledby="cmd-h">
          <h2 id="cmd-h">Run this as root on the server</h2>
          <p className="muted">
            The token in it works once, until {result.expires}. The server shows in Build servers when it connects.
          </p>
          <div className="code-block">
            <pre aria-label="Join command">{shown}</pre>
            <div className="stack">
              <button type="button" onClick={() => navigator.clipboard.writeText(result.command)}>
                Copy
              </button>
              <button type="button" onClick={() => setRevealed((r) => !r)}>
                {revealed ? "Hide" : "Reveal"}
              </button>
            </div>
          </div>
          <Link to="/$org/build-servers" params={{ org }}>
            Back to Build servers
          </Link>
        </section>
      ) : (
        <div className="row">
          <button type="submit" className="primary">
            Make the command
          </button>
          <Link className="button" to="/$org/build-servers" params={{ org }}>
            Cancel
          </Link>
        </div>
      )}
    </form>
  );
}
