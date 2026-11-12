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
};

export function SignIn() {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { error?: string; next?: string };
  const providers = useLoad("providers", () => authApi.listSignInProviders({}));
  const [error, setError] = useState<string>();
  const pending = useAction();

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    try {
      await authApi.signIn({ email: String(f.get("email")), password: String(f.get("password")) });
      const next = search.next?.startsWith("/") && !search.next.startsWith("//") ? search.next : "/";
      navigate({ to: next });
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
          <a key={p.id} className="button" href={`/auth/oidc/${p.id}/start`}>
            Sign in with {p.name}
          </a>
        ))}
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
