import { useEffect, useState, type FormEvent } from "react";
import { ActionButton, Submit, useAction } from "../Action";
import { useNavigate, useParams } from "@tanstack/react-router";
import { authApi, message } from "../api";
import { roleName } from "../roles";
import { useLoad } from "../useLoad";

export function Invite() {
  const { token } = useParams({ strict: false }) as { token: string };
  const navigate = useNavigate();
  const invite = useLoad(token, () => authApi.getInvite({ token }));
  const providers = useLoad("providers", () => authApi.listSignInProviders({}));
  const [signedIn, setSignedIn] = useState<string>();
  const [error, setError] = useState<string>();
  const pending = useAction();

  useEffect(() => {
    authApi.getMe({}).then(
      (me) => setSignedIn(me.user?.email ?? ""),
      () => setSignedIn(""),
    );
  }, []);

  const accept = async (body: { name?: string; password?: string }) => {
    try {
      const r = await authApi.acceptInvite({ token, ...body });
      navigate({ to: "/$org", params: { org: r.orgSlug } });
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    return accept({ name: String(f.get("name")), password: String(f.get("password")) });
  };

  if (invite.error) {
    return (
      <div className="auth-page">
        <div className="card stack">
          <h1>Invite not found</h1>
          <p className="muted">It has expired, was revoked, or was used. Ask an admin for a new one.</p>
        </div>
      </div>
    );
  }
  const inv = invite.data;
  if (!inv || signedIn === undefined) return null;

  return (
    <div className="auth-page">
      <div className="card stack">
        <div>
          <h1>Join {inv.orgName}</h1>
          <p className="muted">
            As {roleName(inv.role)}, for <span className="mono">{inv.email}</span>.
          </p>
        </div>
        {error && <p className="error">{error}</p>}
        {signedIn ? (
          <>
            <p>You are signed in as {signedIn}.</p>
            <ActionButton action={pending} type="button" className="primary" onClick={() => void pending.run(() => accept({}))} busy="Joining…" done="Joined">
              Join {inv.orgName}
            </ActionButton>
          </>
        ) : inv.hasAccount ? (
          <p className="notice">
            You have an account. <a href={`/sign-in?next=${encodeURIComponent(`/invite/${token}`)}`}>Sign in</a>, then
            this page lets you join.
          </p>
        ) : (
          <>
            {providers.data?.providers.map((p) => (
              <a key={p.id} className="button" href={`/auth/oidc/${p.id}/start?invite=${encodeURIComponent(token)}`}>
                Join with {p.name}
              </a>
            ))}
            <form className="stack" onSubmit={(e) => void pending.run(() => submit(e))}>
              <label className="field">
                <span>Your name</span>
                <input name="name" autoComplete="name" />
              </label>
              <label className="field">
                <span>Password</span>
                <input name="password" type="password" required minLength={12} autoComplete="new-password" />
                <span className="hint">12 characters or more.</span>
              </label>
              <Submit action={pending} className="primary" busy="Joining…" done="Joined">
                Create account and join
              </Submit>
            </form>
          </>
        )}
      </div>
    </div>
  );
}
