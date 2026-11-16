import { createContext, useContext } from "react";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";
import { Role } from "./gen/stackwright/v1/orgs_pb";
import type { Project } from "./gen/stackwright/v1/projects_pb";

export type ShellState = {
  me: GetMeResponse;
  projects: Project[] | undefined;
  reloadProjects: () => void;
  waitingVersion: number;
  reloadWaiting: () => void;
  // askAgent opens the workspace ws and its agent panel, and sends text to
  // the workspace's newest session that can take a message, or starts one.
  askAgent: (ws: { org: string; project: string; workspace: string }, text: string) => void;
};

export const ShellContext = createContext<ShellState | undefined>(undefined);

export function useShell(): ShellState {
  const s = useContext(ShellContext);
  if (!s) throw new Error("useShell outside the signed-in app");
  return s;
}

export const useMe = () => useShell().me;

export const useRole = (org: string) => useMe().orgs.find((o) => o.slug === org)?.role ?? Role.UNSPECIFIED;
