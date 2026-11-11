import { useCallback, useEffect, useState } from "react";

export type Tab = { project: string; workspace: string };

const storageKey = (org: string) => `stackwright.tabs.${org}`;
const same = (a: Tab, b: Tab) => a.project === b.project && a.workspace === b.workspace;

function read(org: string): Tab[] {
  try {
    const raw = JSON.parse(window.localStorage.getItem(storageKey(org)) ?? "[]");
    return Array.isArray(raw) ? raw.filter((t) => typeof t?.project === "string" && typeof t?.workspace === "string") : [];
  } catch {
    return [];
  }
}

export function useTabs(org: string) {
  const [tabs, setTabs] = useState<Tab[]>(() => read(org));
  useEffect(() => setTabs(read(org)), [org]);
  const save = useCallback(
    (next: Tab[]) => {
      setTabs(next);
      try {
        window.localStorage.setItem(storageKey(org), JSON.stringify(next));
      } catch {
        // Private windows can refuse storage; the tabs then last until reload.
      }
    },
    [org],
  );
  const open = useCallback(
    (t: Tab) => {
      if (!tabs.some((x) => same(x, t))) save([...tabs, t]);
    },
    [tabs, save],
  );
  const close = useCallback((t: Tab) => save(tabs.filter((x) => !same(x, t))), [tabs, save]);
  return { tabs, open, close };
}
