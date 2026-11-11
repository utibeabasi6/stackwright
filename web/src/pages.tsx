import { useEffect, useState } from "react";
import { meta } from "./api";

export function Projects() {
  return (
    <>
      <h1>Projects</h1>
      <p className="muted">No projects yet.</p>
    </>
  );
}

export function Inbox() {
  return (
    <>
      <h1>Waiting on you</h1>
      <p className="muted">Nothing waits on you.</p>
    </>
  );
}

export function BuildServers() {
  return (
    <>
      <h1>Build servers</h1>
      <p className="muted">No build servers yet.</p>
    </>
  );
}

export function Settings() {
  const [version, setVersion] = useState("…");
  useEffect(() => {
    meta.getVersion({}).then(
      (r) => setVersion(r.version),
      () => setVersion("unknown: the server did not answer"),
    );
  }, []);
  return (
    <>
      <h1>Settings</h1>
      <dl className="facts">
        <dt>Version</dt>
        <dd>{version}</dd>
      </dl>
    </>
  );
}
