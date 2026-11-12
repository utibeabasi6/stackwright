import { useState, type FormEvent } from "react";
import { Submit, useAction } from "../Action";
import { useNavigate } from "@tanstack/react-router";
import { message, orgApi } from "../api";
import { useMe } from "../me";

export function NewOrg() {
  const me = useMe();
  const navigate = useNavigate();
  const [error, setError] = useState<string>();
  const pending = useAction();
  if (!me.user?.instanceAdmin) return <p className="muted">Only an instance admin can make an org.</p>;

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    try {
      const r = await orgApi.createOrg({ name: String(f.get("name")), slug: String(f.get("slug")) });
      window.location.assign(`/${r.slug}`);
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  return (
    <>
      <h1>New org</h1>
      <form className="stack" style={{ maxWidth: 420 }} onSubmit={(e) => void pending.run(() => submit(e))}>
        <label className="field">
          <span>Name</span>
          <input name="name" required />
        </label>
        <label className="field">
          <span>Short name, in links</span>
          <input name="slug" required className="mono" pattern="[a-z0-9]([a-z0-9\-]{0,38}[a-z0-9])?" />
        </label>
        {error && <p className="error">{error}</p>}
        <div className="row">
          <Submit action={pending} className="primary" busy="Creating…" done="Created">
            Create org
          </Submit>
          <button type="button" onClick={() => navigate({ to: "/" })}>
            Cancel
          </button>
        </div>
      </form>
    </>
  );
}
