import { useParams } from "@tanstack/react-router";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useRole } from "../../me";
import { Variables } from "../Variables";

export function OrgVariables() {
  const { org } = useParams({ strict: false }) as { org: string };
  return <Variables org={org} admin={useRole(org) >= Role.ADMIN} />;
}
