import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../../Action";
import { instanceApi, message } from "../../api";
import { SignInProviderKind } from "../../gen/stackwright/v1/instance_pb";
import { useMe } from "../../me";
import { useLoad } from "../../useLoad";
import { Select } from "../../Select";

export function SignInProviders() {
  const me = useMe();
  const providers = useLoad("provider-settings", () =>
    me.user?.instanceAdmin ? instanceApi.listSignInProviderSettings({}) : Promise.resolve(undefined),
  );
  const [kind, setKind] = useState(SignInProviderKind.OIDC);
  const [error, setError] = useState<string>();
  const adding = useAction();
  if (!me.user?.instanceAdmin) return <p className="muted">Only an instance admin manages sign-in.</p>;

  const add = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    setError(undefined);
    try {
      await instanceApi.createSignInProvider({
        kind,
        name: String(f.get("name")),
        issuer: String(f.get("issuer") ?? ""),
        clientId: String(f.get("clientId")),
        clientSecret: String(f.get("clientSecret")),
      });
      form.reset();
      providers.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const remove = async (id: string) => {
    try {
      await instanceApi.deleteSignInProvider({ id });
      providers.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  return (
    <div className="stack">
      <p className="muted">
        These apply to every org on this instance. Only people with an account or an invite can sign in.
      </p>
      {error && <p className="error">{error}</p>}
      {providers.data?.providers.map((p) => (
        <div key={p.id} className="card stack">
          <div className="row" style={{ justifyContent: "space-between", alignItems: "center" }}>
            <strong>{p.name}</strong>
            <Action
              className="danger"
              confirm={{
                message: `Remove ${p.name}? People who signed in only with it lose access until they get a password.`,
                action: "Remove",
                danger: true,
              }}
              run={() => remove(p.id)}
              busy="Removing…"
              done="Removed"
            >
              Remove
            </Action>
          </div>
          <dl className="facts">
            <dt>Kind</dt>
            <dd>{p.kind === SignInProviderKind.GITHUB ? "GitHub" : "OIDC"}</dd>
            {p.issuer && (
              <>
                <dt>Issuer</dt>
                <dd>{p.issuer}</dd>
              </>
            )}
            <dt>Client ID</dt>
            <dd>{p.clientId}</dd>
            <dt>Callback URL</dt>
            <dd>{p.callbackUrl}</dd>
          </dl>
        </div>
      ))}
      <section className="card stack">
        <h2>Add a provider</h2>
        <form className="stack" style={{ maxWidth: 520 }} onSubmit={(e) => void adding.run(() => add(e))}>
          <label className="field">
            <span>Kind</span>
            <Select
              value={String(kind)}
              onChange={(v) => setKind(Number(v) as SignInProviderKind)}
              options={[
                { value: String(SignInProviderKind.OIDC), label: "OIDC: Google, Okta, Keycloak and others" },
                { value: String(SignInProviderKind.GITHUB), label: "GitHub" },
              ]}
            />
          </label>
          <label className="field">
            <span>Name on the sign-in button</span>
            <input name="name" required maxLength={40} />
          </label>
          {kind === SignInProviderKind.OIDC && (
            <label className="field">
              <span>Issuer URL</span>
              <input name="issuer" type="url" required placeholder="https://accounts.google.com" />
            </label>
          )}
          <label className="field">
            <span>Client ID</span>
            <input name="clientId" required />
          </label>
          <label className="field">
            <span>Client secret</span>
            <input name="clientSecret" type="password" required autoComplete="off" />
            <span className="hint">Stored sealed. Nobody can read it back, admins included.</span>
          </label>
          <p className="hint">
            After you add it, copy its callback URL into the provider's app settings. The callback URL appears on the
            provider's card above.
          </p>
          <Submit action={adding} className="primary" busy="Adding…" done="Added">
            Add provider
          </Submit>
        </form>
      </section>
    </div>
  );
}
