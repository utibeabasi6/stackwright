import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../../Action";
import { useParams } from "@tanstack/react-router";
import { message, orgApi } from "../../api";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useMe, useRole } from "../../me";
import { roleName, roles } from "../../roles";
import { useLoad } from "../../useLoad";
import { Select } from "../../Select";

export function Members() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  const myRole = useRole(org);
  const admin = myRole >= Role.ADMIN;
  const members = useLoad(`members:${org}`, () => orgApi.listMembers({ org }));
  const invites = useLoad(`invites:${org}:${admin}`, () =>
    admin ? orgApi.listInvites({ org }) : Promise.resolve(undefined),
  );
  const [error, setError] = useState<string>();
  const [link, setLink] = useState<string>();
  const inviting = useAction();

  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    try {
      await fn();
      members.reload();
      invites.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const offered = roles.filter((r) => r.role !== Role.OWNER || myRole === Role.OWNER);

  const invite = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    await inviting.run(() =>
      act(async () => {
        const r = await orgApi.createInvite({ org, email: String(f.get("email")), role: Number(f.get("role")) as Role });
        setLink(r.url);
        form.reset();
      }),
    );
  };

  return (
    <div className="stack">
      {error && <p className="error">{error}</p>}
      {members.error && <p className="error">{members.error}</p>}
      <div className="table-box">
        <table className="table">
          <thead>
            <tr>
              <th>Member</th>
              <th>Role</th>
              {admin && (
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              )}
            </tr>
          </thead>
          <tbody>
            {members.data?.members.map((m) => (
              <tr key={m.userId}>
                <td>
                  {m.name || m.email}
                  {m.name && <div className="hint">{m.email}</div>}
                </td>
                <td>
                  {admin ? (
                    <Select
                      aria-label={`Role of ${m.email}`}
                      value={String(m.role)}
                      onChange={(v) => act(() => orgApi.setMemberRole({ org, userId: m.userId, role: Number(v) as Role }))}
                      options={roles
                        .filter((r) => r.role === m.role || offered.includes(r))
                        .map((r) => ({ value: String(r.role), label: r.name }))}
                    />
                  ) : (
                    roleName(m.role)
                  )}
                </td>
                {admin && (
                  <td style={{ textAlign: "right" }}>
                    <Action
                      className="danger"
                      confirm={{
                        message: m.userId === me.user?.id ? "Leave this org?" : `Remove ${m.email} from this org?`,
                        action: m.userId === me.user?.id ? "Leave" : "Remove",
                        danger: true,
                      }}
                      run={() => act(() => orgApi.removeMember({ org, userId: m.userId }))}
                      busy={m.userId === me.user?.id ? "Leaving…" : "Removing…"}
                      done={m.userId === me.user?.id ? "Left" : "Removed"}
                    >
                      {m.userId === me.user?.id ? "Leave" : "Remove"}
                    </Action>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {admin && (
        <section className="card stack">
          <h2>Invite someone</h2>
          <form className="row" onSubmit={invite}>
            <label className="field" style={{ flex: "1 1 260px" }}>
              <span>Email</span>
              <input name="email" type="email" required />
            </label>
            <label className="field">
              <span>Role</span>
              <Select name="role" defaultValue={String(Role.WRITER)} options={offered.map((r) => ({ value: String(r.role), label: r.name }))} />
            </label>
            <Submit action={inviting} className="primary" busy="Making the link…" done="Link made">
              Make invite link
            </Submit>
          </form>
          <p className="hint">{roles.map((r) => `${r.name}: ${r.about}`).join(" ")}</p>
          {link && (
            <div className="notice stack">
              <span>Send this link to them. It works once, for 7 days, and is not shown again.</span>
              <div className="copy">
                <input readOnly value={link} aria-label="Invite link" onFocus={(e) => e.currentTarget.select()} />
                <Action run={() => navigator.clipboard.writeText(link)} busy="Copying…" done="Copied">
                  Copy
                </Action>
              </div>
            </div>
          )}
          {invites.data && invites.data.invites.length > 0 && (
            <table className="table">
              <thead>
                <tr>
                  <th>Open invite</th>
                  <th>Role</th>
                  <th>From</th>
                  <th>
                  <span className="sr-only">Actions</span>
                </th>
                </tr>
              </thead>
              <tbody>
                {invites.data.invites.map((i) => (
                  <tr key={i.id}>
                    <td>{i.email}</td>
                    <td>{roleName(i.role)}</td>
                    <td>{i.invitedBy}</td>
                    <td style={{ textAlign: "right" }}>
                      <Action run={() => act(() => orgApi.revokeInvite({ org, id: i.id }))} busy="Revoking…" done="Revoked">
                        Revoke
                      </Action>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      )}
    </div>
  );
}
