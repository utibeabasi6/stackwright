import { useState, type FormEvent } from "react";
import { Submit, useAction } from "../Action";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { authApi, message } from "../api";
import { useLoad } from "../useLoad";

const reasons: Record<string, string> = {
  "no-account": "No account uses that email. Ask an admin for an invite.",
  email: "The provider has no verified email for you.",
  invite: "That invite does not exist, has expired, or is for another email.",
  expired: "The sign-in took too long. Try again.",
  denied: "The provider did not let you sign in.",
  "saml-setup": "That org has no working SAML sign-in. Ask its admin.",
  "saml-response": "The identity provider's answer was not valid for this org.",
  "saml-expired": "The SSO sign-in took too long, or was used already. Try again.",
  "saml-account": "Your email belongs to an account outside this org. Sign in another way, or ask the org's admin.",
  "saml-email": "The identity provider did not send an email address.",
  "saml-internal": "The SSO sign-in failed. Try again.",
  "saml-browser": "The SSO sign-in started in another browser. Start it again here.",
  "saml-owner": "Owners sign in another way, not through the identity provider.",
};

export function SignIn() {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { error?: string; next?: string };
  const providers = useLoad("providers", () => authApi.listSignInProviders({}));
  const [error, setError] = useState<string>();
  const pending = useAction();
  const next = search.next?.startsWith("/") && !search.next.startsWith("//") ? search.next : undefined;

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    try {
      await authApi.signIn({ email: String(f.get("email")), password: String(f.get("password")) });
      navigate({ to: next ?? "/" });
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  const failure = search.error ? (reasons[search.error] ?? "Sign-in failed. Try again.") : undefined;
  return (
    <div className="auth-page">
      <div className="card stack">
        <h1>Sign in</h1>
        {failure && <p className="error">{failure}</p>}
        {providers.data?.providers.map((p) => (
          <a key={p.id} className="button" href={`/auth/oidc/${p.id}/start?next=${encodeURIComponent(next ?? "/")}`}>
            Sign in with {p.name}
          </a>
        ))}
        <form
          className="row"
          aria-label="Single sign-on"
          onSubmit={(e) => {
            e.preventDefault();
            const org = String(new FormData(e.currentTarget).get("org")).trim();
            window.location.assign(`/auth/saml/${encodeURIComponent(org)}/start?next=${encodeURIComponent(next ?? `/${org}`)}`);
          }}
        >
          <label className="field grow">
            <span>Org that signs in with SSO</span>
            <input name="org" className="mono" required pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?" placeholder="acme" />
          </label>
          <button type="submit">Sign in with SSO</button>
        </form>
        <form className="stack" onSubmit={(e) => void pending.run(() => submit(e))}>
          <label className="field">
            <span>Email</span>
            <input name="email" type="email" required autoComplete="email" />
          </label>
          <label className="field">
            <span>Password</span>
            <input name="password" type="password" required autoComplete="current-password" />
          </label>
          {error && <p className="error">{error}</p>}
          <Submit action={pending} className="primary" busy="Signing in…" done="Signed in">
            Sign in
          </Submit>
        </form>
        <p className="hint">Forgot your password? An admin can reset it from the server.</p>
      </div>
    </div>
  );
}
