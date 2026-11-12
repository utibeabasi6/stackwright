import { useParams } from "@tanstack/react-router";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useMe } from "../../me";
import { Variables } from "../Variables";

export function OrgVariables() {
  const { org } = useParams({ strict: false }) as { org: string };
  const me = useMe();
  return <Variables org={org} admin={(me.orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED) >= Role.ADMIN} />;
}
