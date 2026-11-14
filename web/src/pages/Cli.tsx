import { useState } from "react";
import { Link, useSearch } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Action } from "../Action";
import { authApi, message } from "../api";
import { Copy } from "../Copy";
import { useLoad } from "../useLoad";

// Device approves a `stackwright login` from the code it shows (adr/0044).
export function Device() {
  const search = useSearch({ strict: false }) as { code?: string };
  const [code, setCode] = useState(search.code ?? "");
  const [asked, setAsked] = useState(search.code ?? "");
  const [done, setDone] = useState<"approved" | "denied">();
  const device = useLoad(`device:${asked}`, () => (asked ? authApi.getDevice({ userCode: asked }) : Promise.resolve(undefined)));
  if (done) {
    return (
      <div className="stack">
        <h1>CLI sign-in</h1>
        <p className="notice">
          {done === "approved" ? "Approved. The CLI signs in within a few seconds; you can close this page." : "Denied. The CLI stops waiting."}
        </p>
      </div>
    );
  }
  return (
    <div className="stack">
      <h1>CLI sign-in</h1>
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault();
          setAsked(code.trim());
        }}
      >
        <label className="field">
          <span>The code the CLI shows</span>
          <input className="mono" value={code} onChange={(e) => setCode(e.target.value)} placeholder="BCDF-GHJK" autoComplete="off" required />
        </label>
        <button type="submit">Look up</button>
      </form>
      {device.error && <p className="error">{device.error}</p>}
      {device.data && (
        <section className="card stack" aria-label="Sign-in request">
          <p>
            <span className="mono">{device.data.name}</span> asks to sign in as you, with code <span className="mono">{asked.toUpperCase()}</span>.
            The CLI can then do what you can do in every org, until you revoke it. Approve only if you ran{" "}
            <span className="mono">stackwright login</span> just now and the codes match.
          </p>
          <div className="row">
            <Action className="primary" run={() => authApi.approveDevice({ userCode: asked }).then(() => setDone("approved"))} busy="Approving…" done="Approved">
              Approve
            </Action>
            <Action run={() => authApi.denyDevice({ userCode: asked }).then(() => setDone("denied"))} busy="Denying…" done="Denied">
              Deny
            </Action>
          </div>
        </section>
      )}
    </div>
  );
}

const platforms = [
  { name: "stackwright-darwin-arm64", label: "macOS, Apple silicon" },
  { name: "stackwright-darwin-amd64", label: "macOS, Intel" },
  { name: "stackwright-linux-amd64", label: "Linux, x86-64" },
  { name: "stackwright-linux-arm64", label: "Linux, arm64" },
];

// CliPage is where to get the CLI, and the user's CLI sign-ins.
export function CliPage() {
  const tokens = useLoad("cli-tokens", () => authApi.listCliTokens({}).then((r) => r.tokens));
  const [error, setError] = useState<string>();
  const origin = window.location.origin;
  return (
    <div className="stack">
      <div>
        <h1>CLI</h1>
        <p className="muted">
          Plan the working tree from your terminal. A plan from the CLI never applies; the CLI finds the project from the git remote and the
          folder.
        </p>
      </div>
      <section className="card stack" aria-labelledby="install-h">
        <h2 id="install-h">Install</h2>
        <ul className="rows">
          {platforms.map((p) => (
            <li key={p.name} className="stack">
              <div className="row">
                <span className="grow">{p.label}</span>
                <a href={`/cli/${p.name}`} download="stackwright">
                  Download
                </a>
              </div>
              <Copy text={`curl -fLo stackwright ${origin}/cli/${p.name} && chmod +x stackwright`} label={`Install command for ${p.label}`} />
            </li>
          ))}
        </ul>
        <p className="hint">
          Check a download against <a href="/cli/SHA256SUMS">SHA256SUMS</a>. Then sign in:
        </p>
        <Copy text={`stackwright login -server ${origin}`} label="Sign-in command" />
      </section>
      <section className="stack" aria-labelledby="signed-in-h">
        <h2 id="signed-in-h">Signed-in machines</h2>
        {error && <p className="error">{error}</p>}
        {tokens.data?.length === 0 && <p className="muted">No machine is signed in. A machine signs out after 30 days without use.</p>}
        {(tokens.data?.length ?? 0) > 0 && (
          <div className="table-box">
            <table className="table">
              <thead>
                <tr>
                  <th>Machine</th>
                  <th>Signed in</th>
                  <th>Last used</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {tokens.data?.map((t) => (
                  <tr key={t.id}>
                    <td className="mono">{t.name}</td>
                    <td>{t.createdAt ? timestampDate(t.createdAt).toLocaleString() : ""}</td>
                    <td>{t.lastUsedAt ? timestampDate(t.lastUsedAt).toLocaleString() : ""}</td>
                    <td>
                      <Action
                        className="danger"
                        confirm={{ message: `Sign ${t.name} out? Its CLI stops working until it signs in again.`, action: "Sign out", danger: true }}
                        run={() =>
                          authApi
                            .revokeCliToken({ id: t.id })
                            .then(() => tokens.reload())
                            .catch((e) => {
                              setError(message(e));
                              throw e;
                            })
                        }
                        busy="Signing out…"
                        done="Signed out"
                      >
                        Sign out
                      </Action>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="hint">
          <Link to="/">Back to your orgs</Link>
        </p>
      </section>
    </div>
  );
}
