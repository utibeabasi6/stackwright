import { useState, type FormEvent } from "react";
import { useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Action, Submit, useAction } from "../../Action";
import { message, ssoApi } from "../../api";
import { Copy } from "../../Copy";
import { useRole } from "../../me";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { Select } from "../../Select";
import { useLoad } from "../../useLoad";

const roles = [
  { value: String(Role.VIEWER), label: "Viewer" },
  { value: String(Role.SECURITY), label: "Security" },
  { value: String(Role.REVIEWER), label: "Reviewer" },
  { value: String(Role.WRITER), label: "Writer" },
  { value: String(Role.ADMIN), label: "Admin" },
];

type Mapping = { group: string; role: Role };

// Sso sets up the org's SAML identity provider and SCIM (adr/0045).
export function Sso() {
  const { org } = useParams({ strict: false }) as { org: string };
  const sso = useLoad(`sso:${org}`, () => ssoApi.getSso({ org }));
  const owner = useRole(org) === Role.OWNER;
  const cfg = sso.data?.sso;
  const [source, setSource] = useState<"url" | "xml">("url");
  const [mappings, setMappings] = useState<Mapping[] | undefined>();
  const [defaultRole, setDefaultRole] = useState<Role | undefined>();
  const [scimToken, setScimToken] = useState<string>();
  const [error, setError] = useState<string>();
  const saving = useAction();
  const groups = mappings ?? cfg?.groupRoles.map((g) => ({ group: g.group, role: g.role })) ?? [];
  const role = defaultRole ?? cfg?.defaultRole ?? Role.VIEWER;

  const save = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setError(undefined);
    void saving.run(async () => {
      try {
        await ssoApi.saveSso({
          org,
          metadataUrl: source === "url" ? String(f.get("metadataUrl")) : "",
          metadataXml: source === "xml" ? String(f.get("metadataXml")) : "",
          groupsAttribute: String(f.get("groupsAttribute")),
          defaultRole: role,
          required: f.get("required") === "on",
          groupRoles: groups.filter((g) => g.group.trim()),
        });
        setMappings(undefined);
        sso.reload();
      } catch (err) {
        setError(message(err));
        throw err;
      }
    });
  };

  return (
    <div className="stack">
      <p className="muted">
        Members sign in through your SAML identity provider. With SSO required, a member signed in another way cannot open this org; owners
        keep their other sign-in, so a broken identity provider cannot lock you out.
      </p>
      {!owner && <p className="notice">Only an owner changes the identity provider or the SCIM token.</p>}
      {(sso.error || error) && <p className="error">{sso.error || error}</p>}
      {sso.data && (
        <section className="card stack" aria-labelledby="sp-h">
          <h2 id="sp-h">Give your identity provider</h2>
          <Copy label="Entity ID" text={sso.data.spEntityId} />
          <Copy label="Assertion consumer service URL" text={sso.data.acsUrl} />
          <p className="hint">
            The service provider metadata is at <a href={sso.data.spMetadataUrl}>{sso.data.spMetadataUrl}</a>. Send the NameID as the email
            address, and the groups in an attribute.
          </p>
        </section>
      )}
      {cfg && (
        <p className="notice">
          Signed in through <span className="mono">{cfg.idpEntityId}</span>.{" "}
          {cfg.certificatesExpire.map((c, i) => (
            <span key={i}>Its certificate ends {timestampDate(c).toLocaleDateString()}. </span>
          ))}
          {cfg.required ? "SSO is required." : "SSO is offered, not required."}
        </p>
      )}
      <form className="card stack" onSubmit={save} aria-label="Identity provider" key={cfg ? "loaded" : "new"}>
        <h2>Identity provider</h2>
        <fieldset className="stack" disabled={!owner}>
        <fieldset className="row">
          <legend>Metadata</legend>
          <label>
            <input type="radio" checked={source === "url"} onChange={() => setSource("url")} /> From a URL
          </label>
          <label>
            <input type="radio" checked={source === "xml"} onChange={() => setSource("xml")} /> Paste the XML
          </label>
        </fieldset>
        {source === "url" ? (
          <label className="field">
            <span>Metadata URL</span>
            <input name="metadataUrl" type="url" required defaultValue={cfg?.metadataUrl} placeholder="https://idp.example.com/metadata" />
          </label>
        ) : (
          <label className="field">
            <span>Metadata XML</span>
            <textarea name="metadataXml" className="mono" rows={6} required />
          </label>
        )}
        <label className="field">
          <span>Groups attribute</span>
          <input name="groupsAttribute" className="mono" defaultValue={cfg?.groupsAttribute || "groups"} />
        </label>
        <label className="field">
          <span>Default role</span>
          <Select aria-label="Default role" value={String(role)} onChange={(v) => setDefaultRole(Number(v) as Role)} options={roles} />
          <span className="hint">For a member in no mapped group.</span>
        </label>
        <h3>Groups</h3>
        {groups.map((g, i) => (
          <div className="row" key={i}>
            <input aria-label="Group" className="mono" value={g.group} placeholder="platform-admins"
              onChange={(e) => setMappings(groups.map((x, j) => (j === i ? { ...x, group: e.target.value } : x)))} />
            <Select aria-label="Role of the group" value={String(g.role)} options={roles}
              onChange={(v) => setMappings(groups.map((x, j) => (j === i ? { ...x, role: Number(v) as Role } : x)))} />
            <button type="button" className="link" onClick={() => setMappings(groups.filter((_, j) => j !== i))}>
              Remove
            </button>
          </div>
        ))}
        <div>
          <button type="button" onClick={() => setMappings([...groups, { group: "", role: Role.VIEWER }])}>
            Add a group
          </button>
        </div>
        <label>
          <input type="checkbox" name="required" defaultChecked={cfg?.required} /> Require SSO for every member but owners
        </label>
        <div className="row">
          <Submit action={saving} className="primary" busy="Saving…" done="Saved">
            Save
          </Submit>
          {cfg && owner && (
            <Action className="danger" confirm={{ message: "Remove the identity provider? Members sign in the other ways again.", action: "Remove", danger: true }}
              run={() => ssoApi.deleteSso({ org }).then(() => sso.reload())} busy="Removing…" done="Removed">
              Remove
            </Action>
          )}
        </div>
        </fieldset>
      </form>
      {sso.data && (
        <section className="card stack" aria-labelledby="scim-h">
          <h2 id="scim-h">SCIM</h2>
          <p className="muted">
            Your identity provider adds and removes members here. Removing a user removes their access to this org and ends the sessions they
            started through it.
          </p>
          <Copy label="SCIM base URL" text={sso.data.scimUrl} />
          {scimToken && <Copy label="SCIM token, shown once" text={scimToken} />}
          <p className="hint">
            {sso.data.scimTokenCreatedAt
              ? `The token was made ${timestampDate(sso.data.scimTokenCreatedAt).toLocaleString()}. A new one replaces it.`
              : "There is no token yet."}
          </p>
          <div hidden={!owner}>
            <Action run={() => ssoApi.rotateScimToken({ org }).then((r) => (setScimToken(r.token), sso.reload()))} busy="Making…" done="Made">
              {sso.data.scimTokenCreatedAt ? "Replace the token" : "Make a token"}
            </Action>
          </div>
        </section>
      )}
    </div>
  );
}
