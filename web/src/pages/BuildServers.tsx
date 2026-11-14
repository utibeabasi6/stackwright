import { useEffect, useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../Action";
import { Link, useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { buildServerApi, message } from "../api";
import { BuildServer_Status, type BuildServer } from "../gen/stackwright/v1/build_servers_pb";
import { Role } from "../gen/stackwright/v1/orgs_pb";
import { formatLabels, parseLabels } from "../labels";
import { useShell } from "../me";
import { useLoad } from "../useLoad";

export function BuildServers() {
  const { org } = useParams({ strict: false }) as { org: string };
  const { me } = useShell();
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;
  const servers = useLoad(`build-servers:${org}`, () => buildServerApi.listBuildServers({ org }).then((r) => r.buildServers));
  const [editing, setEditing] = useState<string>();
  const [error, setError] = useState<string>();
  const { reload } = servers;

  useEffect(() => {
    const t = setInterval(reload, 5000);
    return () => clearInterval(t);
  }, [reload]);

  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    try {
      await fn();
      setEditing(undefined);
      reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const saving = useAction();
  const remove = (s: BuildServer) => act(() => buildServerApi.removeBuildServer({ org, id: s.id }));
  const save = (s: BuildServer) => (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    void saving.run(() =>
      act(() =>
      buildServerApi.updateBuildServer({
        org,
        id: s.id,
        labels: { values: parseLabels(String(f.get("labels"))) },
        slots: Number(f.get("slots")),
      }),
      ),
    );
  };

  const list = servers.data ?? [];
  const slots = list.reduce((n, s) => n + s.slots, 0);
  const running = list.reduce((n, s) => n + s.running, 0);

  return (
    <div className="stack">
      <div className="row">
        <div className="grow">
          <h2>Build servers</h2>
          <p className="muted">
            {list.length === 1 ? "1 server" : `${list.length} servers`} · {running} of {slots} sandbox slots in use
          </p>
        </div>
        {admin && (
          <Link className="button primary" to="/$org/settings/build-servers/enrol" params={{ org }}>
            Enrol a server
          </Link>
        )}
      </div>
      {(servers.error || error) && <p className="error">{servers.error || error}</p>}

      <div className="table-box">
        <table className="table">
          <thead>
            <tr>
              <th>Server</th>
              <th>Sandboxes</th>
              <th>Labels</th>
              <th>Version</th>
              <th>Status</th>
              {admin && (
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              )}
            </tr>
          </thead>
          <tbody>
            {list.map((s) =>
              editing === s.id ? (
                <tr key={s.id}>
                  <td colSpan={admin ? 6 : 5}>
                    <form className="row" onSubmit={save(s)} aria-label={`Edit ${s.name}`}>
                      <span className="mono">{s.name}</span>
                      <label className="field grow">
                        <span>Labels</span>
                        <input name="labels" defaultValue={formatLabels(s.labels)} placeholder="region=eu" />
                      </label>
                      <label className="field">
                        <span>Most sandboxes at once</span>
                        <input name="slots" type="number" min={1} max={64} defaultValue={s.slots} required />
                      </label>
                      <Submit action={saving} className="primary" busy="Saving…" done="Saved">
                        Save
                      </Submit>
                      <button type="button" onClick={() => setEditing(undefined)}>
                        Cancel
                      </button>
                    </form>
                  </td>
                </tr>
              ) : (
                <tr key={s.id}>
                  <td>
                    <div className="mono">{s.name}</div>
                    <div className="hint">{where(s)}</div>
                  </td>
                  <td className="mono">
                    {s.running} of {s.slots}
                  </td>
                  <td className="mono">{Object.keys(s.labels).length ? formatLabels(s.labels) : <span className="muted">none</span>}</td>
                  <td>
                    <div className="mono muted">{s.version || "—"}</div>
                    {s.upgrade && <div className="hint warn">{s.upgrade}</div>}
                  </td>
                  <td>
                    <Status s={s} />
                  </td>
                  {admin && (
                    <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      {!s.builtIn && (
                        <>
                          <Action
                            run={() =>
                              act(() =>
                                buildServerApi.updateBuildServer({ org, id: s.id, draining: s.status !== BuildServer_Status.DRAINING }),
                              )
                            }
                            busy={s.status === BuildServer_Status.DRAINING ? "Resuming…" : "Draining…"}
                            done={s.status === BuildServer_Status.DRAINING ? "Draining" : "Resumed"}
                          >
                            {s.status === BuildServer_Status.DRAINING ? "Resume" : "Drain"}
                          </Action>{" "}
                          <button type="button" onClick={() => setEditing(s.id)}>
                            Edit
                          </button>{" "}
                          <Action
                            className="danger"
                            confirm={{
                              message: `Remove ${s.name}? Its certificate stops working at once. The builder stays installed on the machine.`,
                              action: "Remove",
                              danger: true,
                            }}
                            run={() => remove(s)}
                            busy="Removing…"
                            done="Removed"
                          >
                            Remove
                          </Action>
                        </>
                      )}
                    </td>
                  )}
                </tr>
              ),
            )}
          </tbody>
        </table>
      </div>

      <aside className="card stack prose" aria-labelledby="placed-h">
        <h2 id="placed-h">How jobs are placed</h2>
        <p>
          Each job goes to an online server with a free slot. A workspace can require labels, set on its Overview. Then its jobs run
          only on a server that has every one of them.
        </p>
        <p>A draining server finishes the jobs it has and takes no new ones.</p>
        <p>
          The built-in server runs on the Stackwright host and serves every org. Its slots and labels come from the instance's{" "}
          <code>.env</code>.
        </p>
      </aside>
    </div>
  );
}

function where(s: BuildServer) {
  if (s.builtIn) return "Same host as Stackwright";
  if (s.sshHost) return `Enrolled over SSH from ${s.sshHost}`;
  return "Enrolled with a command";
}

function Status({ s }: { s: BuildServer }) {
  switch (s.status) {
    case BuildServer_Status.ONLINE:
      return <span className="ok">✓ Online</span>;
    case BuildServer_Status.DRAINING:
      return (
        <>
          <div className="warn">~ Draining</div>
          <div className="hint">{s.running === 1 ? "1 job left" : `${s.running} jobs left`}, then no new ones</div>
        </>
      );
  }
  return (
    <>
      <div className="error">! Offline</div>
      <div className="hint">{s.lastSeenAt ? `Last seen ${timestampDate(s.lastSeenAt).toLocaleString()}` : "Never connected"}</div>
    </>
  );
}
