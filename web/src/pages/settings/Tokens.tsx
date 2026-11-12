import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../../Action";
import { useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { message, tokenApi } from "../../api";
import { Copy } from "../../Copy";
import { useLoad } from "../../useLoad";
import { Select } from "../../Select";

export function Tokens() {
  const { org } = useParams({ strict: false }) as { org: string };
  const tokens = useLoad(`tokens:${org}`, () => tokenApi.listApiTokens({ org }));
  const [secret, setSecret] = useState<string>();
  const [error, setError] = useState<string>();
  const making = useAction();

  const create = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    setError(undefined);
    try {
      const r = await tokenApi.createApiToken({ org, name: String(f.get("name")), expiresInDays: Number(f.get("days")) });
      setSecret(r.secret);
      form.reset();
      tokens.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const revoke = async (id: string, name: string) => {
    if (!window.confirm(`Revoke ${name}? Anything that uses it stops working.`)) return false;
    try {
      await tokenApi.revokeApiToken({ org, id });
      tokens.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  return (
    <div className="stack">
      <p className="muted">
        Your tokens for this org. A token acts as you, with your role now: if your role changes, so does what it can do.
      </p>
      {(tokens.error || error) && <p className="error">{tokens.error || error}</p>}
      {tokens.data && tokens.data.tokens.length > 0 && (
        <div className="table-box">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Expires</th>
                <th>Last used</th>
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tokens.data.tokens.map((t) => (
                <tr key={t.id}>
                  <td>{t.name}</td>
                  <td>{t.expiresAt ? timestampDate(t.expiresAt).toLocaleDateString() : ""}</td>
                  <td>{t.lastUsedAt ? timestampDate(t.lastUsedAt).toLocaleString() : "Never"}</td>
                  <td style={{ textAlign: "right" }}>
                    <Action className="danger" run={() => revoke(t.id, t.name)} busy="Revoking…" done="Revoked">
                      Revoke
                    </Action>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <section className="card stack">
        <h2>New token</h2>
        <form className="row" onSubmit={(e) => void making.run(() => create(e))}>
          <label className="field grow">
            <span>Name</span>
            <input name="name" required maxLength={60} placeholder="laptop, or ci-network" />
          </label>
          <label className="field">
            <span>Expires in</span>
            <Select
              name="days"
              defaultValue="90"
              options={[
                { value: "30", label: "30 days" },
                { value: "90", label: "90 days" },
                { value: "365", label: "1 year" },
              ]}
            />
          </label>
          <Submit action={making} className="primary" busy="Making…" done="Made">
            Make token
          </Submit>
        </form>
        {secret && (
          <div className="notice stack">
            <span>Copy it now. It is not shown again.</span>
            <Copy label="New token" text={secret} />
          </div>
        )}
      </section>
    </div>
  );
}
