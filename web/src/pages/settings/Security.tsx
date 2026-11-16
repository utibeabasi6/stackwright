import { useState, type FormEvent } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useParams } from "@tanstack/react-router";
import { Action, Submit, useAction } from "../../Action";
import { securityApi } from "../../api";
import { useRole } from "../../me";
import { decidesRisk } from "../../roles";
import { useLoad } from "../../useLoad";

const example = `# METADATA
# title: Buckets carry an owner tag
# custom:
#   id: ACME0001
#   avd_id: ACME0001
#   severity: HIGH
#   input:
#     selector:
#       - type: cloud
package user.acme.ACME0001

deny contains res if {
  some b in input.aws.s3.buckets
  not b.tags.owner
  res := result.new("The bucket has no owner tag", b)
}
`;

export function SecuritySettings() {
  const { org } = useParams({ strict: false }) as { org: string };
  const decides = decidesRisk(useRole(org));
  const policy = useLoad(`security-policy:${org}`, () => securityApi.getSecurityPolicy({ org }));
  const [editing, setEditing] = useState<{ name: string; rego: string }>();
  const saving = useAction();
  const skipping = useAction();

  const save = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    void saving.run(async () => {
      await securityApi.setPolicySet({ org, name: String(f.get("name")), rego: String(f.get("rego")) });
      setEditing(undefined);
      policy.reload();
    });
  };
  const remove = async (name: string) => {
    await securityApi.deletePolicySet({ org, name });
    policy.reload();
  };
  const saveSkipped = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const ids = String(new FormData(e.currentTarget).get("skipped"))
      .split(/[\s,]+/)
      .filter(Boolean);
    void skipping.run(async () => {
      await securityApi.setSkippedChecks({ org, skippedChecks: ids });
      policy.reload();
    });
  };

  const sets = policy.data?.policySets ?? [];
  return (
    <div className="stack">
      <p className="muted">
        Every plan runs Trivy's built-in checks and the policy sets below in the sandbox, with no network. The security role, admins and
        owners change this page and accept risks.
      </p>
      {policy.error && <p className="error">{policy.error}</p>}

      <section className="stack" aria-labelledby="sets-h">
        <h2 id="sets-h">Policy sets</h2>
        {sets.length === 0 && <p className="muted">No policy sets. Only the built-in checks run.</p>}
        {sets.length > 0 && (
          <div className="table-box">
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Updated</th>
                  {decides && (
                    <th>
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {sets.map((p) => (
                  <tr key={p.name}>
                    <td className="mono">{p.name}</td>
                    <td className="muted">
                      {p.updatedAt && timestampDate(p.updatedAt).toLocaleString()}
                      {p.updatedBy && ` by ${p.updatedBy}`}
                    </td>
                    {decides && (
                      <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                        <button type="button" onClick={() => setEditing({ name: p.name, rego: p.rego })}>
                          Edit
                        </button>{" "}
                        <Action
                          className="danger"
                          confirm={{
                            message: `Delete the policy set ${p.name}? Its findings close on each workspace's next plan.`,
                            action: "Delete",
                            danger: true,
                          }}
                          run={() => remove(p.name)}
                          busy="Deleting…"
                          done="Deleted"
                        >
                          Delete
                        </Action>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {decides && !editing && (
          <div>
            <button type="button" onClick={() => setEditing({ name: "", rego: example })}>
              New policy set
            </button>
          </div>
        )}
        {decides && editing && (
          <form className="card stack" onSubmit={save} key={editing.name}>
            <label className="field">
              Name
              <input name="name" required defaultValue={editing.name} readOnly={!!editing.name} pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?" />
            </label>
            <label className="field">
              Rego, in Trivy's check format
              <textarea name="rego" required rows={16} className="mono" defaultValue={editing.rego} spellCheck={false} />
            </label>
            <span className="hint">
              The package must be under <code>user.</code>. The check reads Trivy's cloud model of the plan, such as{" "}
              <code>input.aws.s3.buckets</code>.
            </span>
            <div className="row">
              <Submit action={saving} className="primary" busy="Saving…" done="Saved">
                Save policy set
              </Submit>
              <button type="button" onClick={() => setEditing(undefined)}>
                Cancel
              </button>
            </div>
          </form>
        )}
      </section>

      <section className="stack" aria-labelledby="skipped-h">
        <h2 id="skipped-h">Skipped checks</h2>
        <p className="muted">Built-in checks that never run in this org, such as AWS-0089. A skipped check's findings close on the next plan.</p>
        {policy.data && (
          <form className="stack" onSubmit={saveSkipped} key={policy.data.skippedChecks.join(",")}>
            <label className="field">
              Check ids, one for each line
              <textarea name="skipped" rows={4} className="mono" defaultValue={policy.data.skippedChecks.join("\n")} readOnly={!decides} />
            </label>
            {decides && (
              <div>
                <Submit action={skipping} busy="Saving…" done="Saved">
                  Save skipped checks
                </Submit>
              </div>
            )}
          </form>
        )}
      </section>
    </div>
  );
}
