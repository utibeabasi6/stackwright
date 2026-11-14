import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../Action";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { message, variableApi } from "../api";
import { VariableCategory, type Variable } from "../gen/stackwright/v1/variables_pb";
import { useLoad } from "../useLoad";
import { Select } from "../Select";

type Props = { org: string; project?: string; workspace?: string; admin: boolean };

const categoryName: Record<VariableCategory, string> = {
  [VariableCategory.UNSPECIFIED]: "",
  [VariableCategory.TERRAFORM]: "Input",
  [VariableCategory.ENV]: "Environment",
};

function scopeOf(v: Variable) {
  if (v.scope?.workspace) return "This workspace";
  if (v.scope?.project) return `Project ${v.scope.project}`;
  return "Org";
}

// Variables lists a workspace's effective variables, or the org's own when
// no workspace is given. An admin sets them at the workspace, the project
// or the org.
export function Variables({ org, project, workspace, admin }: Props) {
  const effective = Boolean(workspace);
  const list = useLoad(`variables:${org}/${project ?? ""}/${workspace ?? ""}`, () =>
    variableApi.listVariables({ org, scope: { project, workspace }, effective }).then((r) => r.variables),
  );
  const [error, setError] = useState<string>();
  const setting = useAction();

  const set = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    const sensitive = f.get("sensitive") === "on";
    const value = String(f.get("value"));
    const target = String(f.get("scope") ?? "org");
    setError(undefined);
    try {
      await variableApi.setVariable({
        org,
        scope: target === "workspace" ? { project, workspace } : target === "project" ? { project } : {},
        category: Number(f.get("category")) as VariableCategory,
        key: String(f.get("key")),
        value,
        sensitive,
        keepValue: sensitive && value === "",
        description: String(f.get("description")),
      });
      form.reset();
      list.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  const remove = async (v: Variable) => {
    setError(undefined);
    try {
      await variableApi.deleteVariable({ org, scope: v.scope ?? {}, category: v.category, key: v.key });
      list.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const mine = (v: Variable) => !effective || Boolean(v.scope?.project);

  return (
    <section className="card stack" aria-labelledby="vars-h">
      <h2 id="vars-h">Variables</h2>
      <p className="muted">
        {effective
          ? "What each run here gets. A workspace's value wins over the project's, which wins over the org's."
          : "Every workspace in the org gets these, unless its project or the workspace sets the same one."}{" "}
        Sensitive values are sealed, never shown again, and masked in all output.
      </p>
      {(list.error || error) && <p className="error">{list.error || error}</p>}
      {list.data?.length === 0 && <p className="muted">No variables yet.</p>}
      {(list.data?.length ?? 0) > 0 && (
        <div className="table-box">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>Value</th>
                {effective && <th>Set at</th>}
                <th>Changed</th>
                {admin && (
                  <th>
                    <span className="sr-only">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {list.data?.map((v) => (
                <tr key={`${v.category}:${v.key}`}>
                  <td>
                    <div className="mono">{v.key}</div>
                    {v.description && <div className="hint">{v.description}</div>}
                  </td>
                  <td>{categoryName[v.category]}</td>
                  <td className="mono">{v.sensitive ? <span className="muted">Sensitive, hidden</span> : v.value}</td>
                  {effective && <td>{scopeOf(v)}</td>}
                  <td className="hint">
                    {v.updatedAt ? timestampDate(v.updatedAt).toLocaleDateString() : ""}
                    {v.updatedBy && ` · ${v.updatedBy}`}
                  </td>
                  {admin && (
                    <td style={{ textAlign: "right" }}>
                      {mine(v) && (
                        <Action
                          className="danger"
                          confirm={{ message: `Delete ${v.key}? Runs stop getting it.`, action: "Delete", danger: true }}
                          run={() => remove(v)}
                          busy="Deleting…"
                          done="Deleted"
                        >
                          Delete
                        </Action>
                      )}
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {admin && (
        <form className="stack" onSubmit={(e) => void setting.run(() => set(e))} aria-label="Set a variable">
          <div className="row">
            <label className="field">
              <span>Kind</span>
              <Select
                name="category"
                defaultValue={String(VariableCategory.TERRAFORM)}
                options={[
                  { value: String(VariableCategory.TERRAFORM), label: "Input variable" },
                  { value: String(VariableCategory.ENV), label: "Environment variable" },
                ]}
              />
            </label>
            <label className="field">
              <span>Name</span>
              <input name="key" required className="mono" placeholder="region" />
            </label>
            <label className="field grow">
              <span>Value</span>
              <input name="value" className="mono" autoComplete="off" />
            </label>
            {effective && (
              <label className="field">
                <span>Set at</span>
                <Select
                  name="scope"
                  defaultValue="workspace"
                  options={[
                    { value: "workspace", label: "This workspace" },
                    { value: "project", label: `Project ${project}` },
                  ]}
                />
              </label>
            )}
          </div>
          <div className="row">
            <label className="field grow">
              <span>Description (optional)</span>
              <input name="description" maxLength={200} />
            </label>
            <label className="check">
              <input name="sensitive" type="checkbox" /> Sensitive
            </label>
            <Submit action={setting} busy="Setting…" done="Set">
              Set variable
            </Submit>
          </div>
          <p className="hint">
            Setting a name that exists replaces it. For a sensitive one, leave the value empty to keep the sealed value and change only
            its description.
          </p>
        </form>
      )}
    </section>
  );
}
