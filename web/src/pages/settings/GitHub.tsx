import { useState, type FormEvent } from "react";
import { Action, Submit, useAction } from "../../Action";
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
  const creating = useAction();

  const act = async (fn: () => Promise<unknown>) => {
    setError(undefined);
    try {
      await fn();
      gh.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };
  const createApp = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const owner = String(new FormData(e.currentTarget).get("owner")).trim();
    void creating.run(() =>
      act(async () => {
        const r = await githubApi.startAppManifest({ githubOrganization: owner, returnTo: `/${org}/settings/github` });
        postForm(r.url, r.manifest);
      }),
    );
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
                <Submit action={creating} className="primary" busy="Opening GitHub…" done="Opening GitHub…">
                  Create the GitHub App
                </Submit>
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
              <Action className="primary" run={install} busy="Opening GitHub…" done="Opening GitHub…">
                Install on GitHub
              </Action>
            )}
            {admin && (gh.data?.installations.length ?? 0) > 0 && (
              <Action run={() => act(() => githubApi.refreshRepos({ org }))} busy="Refreshing…" done="Refreshed">
                Refresh repositories
              </Action>
            )}
            {instanceAdmin && (
              <Action
                className="danger"
                confirm={{
                  message: "Forget the GitHub App? Every org's links to it go. The App stays on GitHub until you delete it there.",
                  action: "Forget",
                  danger: true,
                }}
                run={() => act(() => githubApi.deleteGitHubApp({}))}
                busy="Forgetting…"
                done="Forgotten"
              >
                Forget the App
              </Action>
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
              <Action
                className="danger"
                confirm={{ message: `Unlink ${i.account}? Its pull requests and pushes stop starting runs.`, action: "Unlink", danger: true }}
                run={() => act(() => githubApi.unlinkInstallation({ org, installationId: i.installationId }))}
                busy="Unlinking…"
                done="Unlinked"
              >
                Unlink
              </Action>
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
