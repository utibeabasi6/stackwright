import { Action } from "../Action";
import { Link } from "@tanstack/react-router";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { stateApi } from "../api";
import { Copy } from "../Copy";
import { Engine } from "../gen/stackwright/v1/projects_pb";
import { useLoad } from "../useLoad";

const when = (t?: Timestamp) => (t ? timestampDate(t).toLocaleString() : "");

export function StatePanel(props: { org: string; project: string; workspace: string; engine: Engine; admin: boolean }) {
  const { org, project, workspace, admin } = props;
  const key = `${org}/${project}/${workspace}`;
  const st = useLoad(`state:${key}`, () => stateApi.getState({ org, project, workspace }));
  const history = useLoad(`history:${key}`, () => stateApi.listStateVersions({ org, project, workspace }));
  const cli = props.engine === Engine.TERRAFORM ? "terraform" : "tofu";

  const act = async (fn: () => Promise<unknown>) => {
    await fn();
    st.reload();
    history.reload();
  };
  const download = async (id: string, serial: bigint) => {
    const r = await stateApi.getStateVersion({ org, project, workspace, id });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([r.state], { type: "application/json" }));
    a.download = `${project}-${workspace}-${serial}.tfstate`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  const latest = st.data?.latest;
  const lock = st.data?.lock;
  return (
    <>
      <section className="card" aria-labelledby="state-h">
        <h2 id="state-h">State</h2>
        {st.error && <p className="error">{st.error}</p>}
        <dl className="facts">
          <dt>Version</dt>
          <dd>{latest ? `serial ${latest.serial}` : "No state yet"}</dd>
          {latest && (
            <>
              <dt>Resources</dt>
              <dd>{latest.resources}</dd>
              <dt>Written</dt>
              <dd>
                {when(latest.createdAt)}
                {latest.createdBy && `, by ${latest.createdBy}`}
              </dd>
            </>
          )}
          <dt>Lock</dt>
          <dd className={lock ? "warn" : "ok"}>
            {lock
              ? `~ Locked by ${lock.who || lock.lockedBy}${lock.operation ? ` for ${lock.operation.replace("OperationType", "").toLowerCase()}` : ""}, ${when(lock.createdAt)}`
              : "✓ Not locked"}
          </dd>
        </dl>
        {lock && admin && (
          <Action
            className="danger"
            confirm={{
              message: "Force-unlock this state? Only do it when the run that holds the lock has stopped.",
              action: "Force unlock",
              danger: true,
            }}
            run={() => act(() => stateApi.forceUnlockState({ org, project, workspace }))}
            busy="Unlocking…"
            done="Unlocked"
          >
            Force unlock
          </Action>
        )}
      </section>

      {st.data && (
        <details className="card">
          <summary>Use this workspace from your laptop or CI</summary>
          <ol className="setup">
            <li>
              Add an empty <code>http</code> backend to your code:
              <Copy label="Backend block" text={'terraform {\n  backend "http" {}\n}\n'} />
            </li>
            <li>
              Save this as <code>{workspace}.tfbackend</code> next to it:
              <Copy label="Backend config" text={st.data.backendConfig} />
            </li>
            <li>
              Make a token in{" "}
              <Link to="/$org/settings/tokens" params={{ org }}>
                Settings › API tokens
              </Link>{" "}
              and put it in your environment, never in a file:
              <Copy label="Token" text={"export TF_HTTP_PASSWORD=<your token>\n"} />
            </li>
            <li>
              Point {cli} at this workspace:
              <Copy label="Init command" text={`${cli} init -reconfigure -backend-config=${workspace}.tfbackend\n`} />
            </li>
          </ol>
          <p className="hint">Each workspace has its own address, so use one backend config file for each.</p>
        </details>
      )}

      {history.data && history.data.versions.length > 0 && (
        <section className="card" aria-labelledby="history-h">
          <h2 id="history-h">State history</h2>
          <div className="table-box">
            <table className="table">
              <thead>
                <tr>
                  <th>Serial</th>
                  <th>Resources</th>
                  <th>Written</th>
                  <th>By</th>
                  <th>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {history.data.versions.map((v, i) => (
                  <tr key={v.id}>
                    <td className="mono">
                      {String(v.serial)}
                      {v.rolledBackFromSerial > 0n && <span className="hint"> rollback to {String(v.rolledBackFromSerial)}</span>}
                    </td>
                    <td>{v.resources}</td>
                    <td>{when(v.createdAt)}</td>
                    <td>{v.createdBy}</td>
                    <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      {admin && (
                        <Action run={() => download(v.id, v.serial)} busy="Downloading…" done="Downloaded">
                          Download
                        </Action>
                      )}{" "}
                      {admin && i > 0 && (
                        <Action
                          confirm={{
                            message: `Make serial ${v.serial} the current state again? The next plan compares your code against it.`,
                            action: "Roll back",
                          }}
                          run={() => act(() => stateApi.rollbackState({ org, project, workspace, id: v.id }))}
                          busy="Rolling back…"
                          done="Rolled back"
                        >
                          Roll back to this
                        </Action>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </>
  );
}
