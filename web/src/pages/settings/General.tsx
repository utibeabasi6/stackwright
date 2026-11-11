import { useParams } from "@tanstack/react-router";
import { meta } from "../../api";
import { useMe } from "../../me";
import { roleName } from "../../roles";
import { useLoad } from "../../useLoad";

export function General() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  const version = useLoad("version", () => meta.getVersion({}));
  const membership = me.orgs.find((o) => o.slug === org);
  return (
    <dl className="facts">
      <dt>Org</dt>
      <dd>
        {membership?.name} <span className="muted">({org})</span>
      </dd>
      <dt>Your role</dt>
      <dd>{membership ? roleName(membership.role) : "None"}</dd>
      <dt>Version</dt>
      <dd>{version.data?.version ?? version.error ?? "…"}</dd>
    </dl>
  );
}
