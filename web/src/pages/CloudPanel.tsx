import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { cloudApi, message } from "../api";
import { useLoad } from "../useLoad";
import { Select } from "../Select";

type Props = { org: string; project: string; workspace: string; account: string; admin: boolean; reload: () => void };

export function CloudPanel({ org, project, workspace, account, admin, reload }: Props) {
  const accounts = useLoad(`cloud:${org}`, () => cloudApi.listCloudAccounts({ org }));
  const [error, setError] = useState<string>();
  const current = accounts.data?.accounts.find((a) => a.name === account);

  const choose = async (name: string) => {
    setError(undefined);
    try {
      await cloudApi.setWorkspaceCloudAccount({ org, project, workspace, account: name });
      reload();
      accounts.reload();
    } catch (err) {
      setError(message(err));
    }
  };

  return (
    <section className="card stack" aria-labelledby="cloud-h">
      <h2 id="cloud-h">Cloud credentials</h2>
      {current ? (
        <p>
          Runs get AWS credentials from <span className="mono">{current.name}</span> in {current.aws?.region}: plans as{" "}
          <span className="mono">{current.aws?.planRoleArn}</span>, applies as <span className="mono">{current.aws?.applyRoleArn}</span>.
        </p>
      ) : (
        <p className="muted">Runs get no cloud credentials.</p>
      )}
      {current && (
        <p className="hint">If you just chose it, deploy the account's AWS setup again so its roles trust this workspace.</p>
      )}
      {error && <p className="error">{error}</p>}
      {admin && (accounts.data?.accounts.length ?? 0) > 0 && (
        <label className="field">
          <span>Cloud account</span>
          <Select
            value={account}
            onChange={choose}
            options={[{ value: "", label: "None" }, ...(accounts.data?.accounts ?? []).map((a) => ({ value: a.name, label: a.name }))]}
          />
        </label>
      )}
      {admin && accounts.data?.accounts.length === 0 && (
        <Link to="/$org/settings/cloud" params={{ org }}>
          Add a cloud account in Settings
        </Link>
      )}
    </section>
  );
}
