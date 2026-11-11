import { createContext, useContext } from "react";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";
import type { Project } from "./gen/stackwright/v1/projects_pb";
import type { Tab } from "./tabs";

export type ShellState = {
  me: GetMeResponse;
  projects: Project[] | undefined;
  reloadProjects: () => void;
  openTab: (t: Tab) => void;
};

export const ShellContext = createContext<ShellState | undefined>(undefined);

export function useShell(): ShellState {
  const s = useContext(ShellContext);
  if (!s) throw new Error("useShell outside the signed-in app");
  return s;
}

export const useMe = () => useShell().me;
