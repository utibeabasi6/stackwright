import { useState, type FormEvent } from "react";
import { useNavigate } from "@tanstack/react-router";
import { authApi, message } from "../api";

const slugOf = (name: string) =>
  name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40);

export function Setup() {
  const navigate = useNavigate();
  const [orgName, setOrgName] = useState("");
  const [orgSlug, setOrgSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setBusy(true);
    try {
      const r = await authApi.completeSetup({
        setupCode: String(f.get("code")),
        orgName,
        orgSlug,
        email: String(f.get("email")),
        name: String(f.get("name")),
        password: String(f.get("password")),
      });
      navigate({ to: "/$org", params: { org: r.orgSlug } });
    } catch (err) {
      setError(message(err));
      setBusy(false);
    }
  };

  return (
    <div className="auth-page">
      <div className="card stack">
        <div>
          <h1>Set up Stackwright</h1>
          <p className="muted">You will be the first admin of this instance.</p>
        </div>
        <form className="stack" onSubmit={submit}>
          <label className="field">
            <span>Setup code</span>
            <input name="code" required autoComplete="off" className="mono" />
            <span className="hint">
              The server prints it once at start. Find it with{" "}
              <code>docker compose logs stackwright | grep setup_code</code>. It stops anyone else on your network from
              claiming this instance.
            </span>
          </label>
          <label className="field">
            <span>Organisation name</span>
            <input
              required
              value={orgName}
              onChange={(e) => {
                setOrgName(e.target.value);
                if (!slugEdited) setOrgSlug(slugOf(e.target.value));
              }}
            />
          </label>
          <label className="field">
            <span>Short name, in links</span>
            <input
              required
              className="mono"
              value={orgSlug}
              pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?"
              onChange={(e) => {
                setSlugEdited(true);
                setOrgSlug(e.target.value);
              }}
            />
          </label>
          <label className="field">
            <span>Your name</span>
            <input name="name" autoComplete="name" />
          </label>
          <label className="field">
            <span>Your email</span>
            <input name="email" type="email" required autoComplete="email" />
          </label>
          <label className="field">
            <span>Password</span>
            <input name="password" type="password" required minLength={12} autoComplete="new-password" />
            <span className="hint">12 characters or more. You can add GitHub or OIDC sign-in later in Settings.</span>
          </label>
          {error && <p className="error">{error}</p>}
          <button type="submit" className="primary" disabled={busy}>
            Create admin and org
          </button>
        </form>
      </div>
    </div>
  );
}
