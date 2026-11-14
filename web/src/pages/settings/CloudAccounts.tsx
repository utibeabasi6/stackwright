import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../../Action";
import { useParams } from "@tanstack/react-router";
import { cloudApi, message } from "../../api";
import { Copy } from "../../Copy";
import type { GetAwsSetupResponse } from "../../gen/stackwright/v1/cloud_pb";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useMe } from "../../me";
import { useLoad } from "../../useLoad";

export function CloudAccounts() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;
  const accounts = useLoad(`cloud:${org}`, () => cloudApi.listCloudAccounts({ org }));
  const [setup, setSetup] = useState<{ name: string; data: GetAwsSetupResponse }>();
  const [error, setError] = useState<string>();
  const adding = useAction();

  const create = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    setError(undefined);
    try {
      await cloudApi.createCloudAccount({
        org,
        name: String(f.get("name")),
        aws: { planRoleArn: String(f.get("plan")), applyRoleArn: String(f.get("apply")), region: String(f.get("region")) },
      });
      form.reset();
      accounts.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const showSetup = async (name: string) => {
    setError(undefined);
    try {
      setSetup({ name, data: await cloudApi.getAwsSetup({ org, account: name }) });
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const remove = async (name: string) => {
    setError(undefined);
    try {
      await cloudApi.deleteCloudAccount({ org, name });
      if (setup?.name === name) setSetup(undefined);
      accounts.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  const issuer = accounts.data?.issuer;
  return (
    <div className="stack">
      <p className="muted">
        A run gets short-lived AWS credentials for the role of its phase: plans assume the plan role, applies the apply role.
        Stackwright stores no keys. AWS trusts a token that Stackwright signs for each job.
      </p>
      {accounts.data && !issuer && (
        <p className="notice">
          AWS trusts only an https issuer, and this instance has none. Set <code>STACKWRIGHT_OIDC_ISSUER</code> (or{" "}
          <code>STACKWRIGHT_PUBLIC_URL</code>) to an https URL that serves <code>/.well-known/openid-configuration</code>.
        </p>
      )}
      {(accounts.error || error) && <p className="error">{accounts.error || error}</p>}
      {(accounts.data?.accounts.length ?? 0) > 0 && (
        <div className="table-box">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Roles</th>
                <th>Region</th>
                <th>Workspaces</th>
                {admin && (
                  <th>
                    <span className="sr-only">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {accounts.data?.accounts.map((a) => (
                <tr key={a.id}>
                  <td className="mono">{a.name}</td>
                  <td className="mono">
                    <div>plan {a.aws?.planRoleArn}</div>
                    <div>apply {a.aws?.applyRoleArn}</div>
                  </td>
                  <td className="mono">{a.aws?.region}</td>
                  <td className="mono">{a.workspaces.length ? a.workspaces.join(", ") : <span className="muted">none yet</span>}</td>
                  {admin && (
                    <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      <Action run={() => showSetup(a.name)} busy="Loading…" done="Shown below">
                        AWS setup
                      </Action>{" "}
                      <Action
                        className="danger"
                        confirm={{ message: `Delete the cloud account ${a.name}? Its roles in AWS stay as they are.`, action: "Delete", danger: true }}
                        run={() => remove(a.name)}
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

      {setup && (
        <section className="card stack" aria-labelledby="setup-h">
          <h2 id="setup-h">AWS setup for {setup.name}</h2>
          <p className="muted">
            Deploy this CloudFormation template in the account. It trusts <code>{setup.data.issuer}</code> and names each workspace that
            uses this account, so deploy it again after you add or remove one. Plans get <code>ReadOnlyAccess</code> unless you change
            it.
          </p>
          <Copy label="CloudFormation template" text={setup.data.cloudformationTemplate} />
          <details>
            <summary>Trust policies alone</summary>
            <Copy label="Plan role trust policy" text={setup.data.planRoleTrustPolicy} />
            <Copy label="Apply role trust policy" text={setup.data.applyRoleTrustPolicy} />
          </details>
        </section>
      )}

      {admin && (
        <section className="card stack" aria-labelledby="new-account-h">
          <h2 id="new-account-h">New AWS account</h2>
          <form className="stack" onSubmit={(e) => void adding.run(() => create(e))}>
            <div className="row">
              <label className="field">
                <span>Name</span>
                <input name="name" required placeholder="prod-aws" />
              </label>
              <label className="field">
                <span>Region</span>
                <input name="region" required className="mono" placeholder="eu-west-1" />
              </label>
            </div>
            <label className="field">
              <span>Plan role ARN</span>
              <input name="plan" required className="mono" placeholder="arn:aws:iam::123456789012:role/stackwright-plan" />
            </label>
            <label className="field">
              <span>Apply role ARN</span>
              <input name="apply" required className="mono" placeholder="arn:aws:iam::123456789012:role/stackwright-apply" />
            </label>
            <p className="hint">The roles need not exist yet: the AWS setup makes them once a workspace uses the account.</p>
            <div className="row">
              <Submit action={adding} className="primary" busy="Adding…" done="Added">
                Add account
              </Submit>
            </div>
          </form>
        </section>
      )}
    </div>
  );
}
