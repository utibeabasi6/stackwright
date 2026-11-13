import { Role } from "./gen/stackwright/v1/orgs_pb";

export const roles = [
  { role: Role.VIEWER, name: "Viewer", about: "Reads everything." },
  { role: Role.SECURITY, name: "Security", about: "Viewer, and accepts security risks." },
  { role: Role.REVIEWER, name: "Reviewer", about: "Viewer, and approves plans." },
  { role: Role.WRITER, name: "Writer", about: "Reviewer, and starts plans and agent sessions." },
  { role: Role.ADMIN, name: "Admin", about: "Writer, and manages members and settings." },
  { role: Role.OWNER, name: "Owner", about: "Admin, and makes or removes owners." },
];

// decidesRisk says who accepts risks and edits the security policy: the
// security role, admins and owners, but not writers or reviewers.
export const decidesRisk = (r: Role) => r === Role.SECURITY || r >= Role.ADMIN;

export const roleName = (r: Role) => roles.find((x) => x.role === r)?.name ?? "Unknown";
