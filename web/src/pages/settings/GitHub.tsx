import { useState, type FormEvent } from "react";
import { useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { githubApi, message } from "../../api";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useMe } from "../../me";
import { useLoad } from "../../useLoad";

// postForm sends the browser to GitHub with the App manifest, which
// GitHub takes only as a form post.
function postForm(url: string, manifest: string) {
  const form = document.createElement("form");
  form.method = "post";
  form.action = url;
  const field = document.createElement("input");
  field.type = "hidden";
  field.name = "manifest";
  field.value = manifest;
  form.append(field);
  document.body.append(form);
  form.submit();
}

export function GitHub() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  const admin = (me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN;
  const instanceAdmin = Boolean(me.user?.instanceAdmin);
  const gh = useLoad(`github:${org}`, () => githubApi.getGitHub({ org }));
  const [error, setError] = useState<string | undefined>(() => new URLSearchParams(window.location.search).get("error") ?? undefined);
  const [busy, setBusy] = useState(false);

  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    setBusy(true);
    try {
      await fn();
      gh.reload();
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  const createApp = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const owner = String(new FormData(e.currentTarget).get("owner")).trim();
    void act(async () => {
      const r = await githubApi.startAppManifest({ githubOrganization: owner, returnTo: `/${org}/settings/github` });
      postForm(r.url, r.manifest);
    });
  };
  const install = () =>
    act(async () => {
      window.location.href = (await githubApi.startInstall({ org })).url;
    });

  const app = gh.data?.app;
  return (
    <div className="stack">
      <p className="muted">
        A pull request plans the workspaces it touches, and a push to a project's branch plans and applies them under each
        workspace's rules. Pull requests from forks never plan.
      </p>
      {(gh.error || error) && <p className="error">{gh.error || error}</p>}

      {gh.data && !app && (
        <section className="card stack" aria-labelledby="app-h">
          <h2 id="app-h">The GitHub App</h2>
          {instanceAdmin ? (
            <form className="stack" onSubmit={createApp}>
              <p className="muted">
                Stackwright makes a private GitHub App for this instance. GitHub shows its settings, and you confirm them there. Its
                webhook URL is this instance's public URL, which GitHub must reach.
              </p>
              <label className="field">
                <span>GitHub organization to own it (optional)</span>
                <input name="owner" placeholder="Empty: your own GitHub account" />
              </label>
              <div className="row">
                <button type="submit" className="primary" disabled={busy}>
                  Create the GitHub App
                </button>
              </div>
            </form>
          ) : (
            <p className="muted">An instance admin must create the GitHub App first.</p>
          )}
        </section>
      )}

      {app && (
        <section className="card stack" aria-labelledby="app-h">
          <h2 id="app-h">The GitHub App</h2>
          <dl className="facts">
            <dt>App</dt>
            <dd>
              <a href={app.htmlUrl} target="_blank" rel="noreferrer">
                {app.name}
              </a>
            </dd>
            <dt>Owner</dt>
            <dd>{app.owner}</dd>
          </dl>
          <div className="row">
            {admin && (
              <button type="button" className="primary" onClick={install} disabled={busy}>
                Install on GitHub
              </button>
            )}
            {admin && (gh.data?.installations.length ?? 0) > 0 && (
              <button type="button" onClick={() => act(() => githubApi.refreshRepos({ org }))} disabled={busy}>
                Refresh repositories
              </button>
            )}
            {instanceAdmin && (
              <button
                type="button"
                className="danger"
                onClick={() =>
                  window.confirm("Forget the GitHub App? Every org's links to it go. The App stays on GitHub until you delete it there.") &&
                  act(() => githubApi.deleteGitHubApp({}))
                }
              >
                Forget the App
              </button>
            )}
          </div>
          {admin && (
            <p className="hint">
              After you install it, GitHub asks you to authorize the App. That proves your GitHub account can see the installation
              before Stackwright links it to this org.
            </p>
          )}
        </section>
      )}

      {gh.data?.installations.map((i) => (
        <section key={String(i.installationId)} className="card stack" aria-label={`Installation on ${i.account}`}>
          <div className="row">
            <h2 className="grow">{i.account}</h2>
            <span className="hint">
              Linked {i.linkedAt ? timestampDate(i.linkedAt).toLocaleDateString() : ""}
              {i.linkedBy && ` by ${i.linkedBy}`}
            </span>
            {admin && (
              <button
                type="button"
                className="danger"
                onClick={() =>
                  window.confirm(`Unlink ${i.account}? Its pull requests and pushes stop starting runs.`) &&
                  act(() => githubApi.unlinkInstallation({ org, installationId: i.installationId }))
                }
              >
                Unlink
              </button>
            )}
          </div>
          {i.repos.length === 0 ? (
            <p className="muted">The installation gives Stackwright no repositories.</p>
          ) : (
            <div className="table-box">
              <table className="table">
                <thead>
                  <tr>
                    <th>Repository</th>
                    <th>Default branch</th>
                    <th>Projects</th>
                  </tr>
                </thead>
                <tbody>
                  {i.repos.map((r) => (
                    <tr key={r.fullName}>
                      <td className="mono">{r.fullName}</td>
                      <td className="mono">{r.defaultBranch}</td>
                      <td className="mono">
                        {r.projects.length ? r.projects.join(", ") : <span className="muted">none; set a project's repository URL</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ))}
    </div>
  );
}
