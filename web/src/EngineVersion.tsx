import { useEffect } from "react";
import { projectApi } from "./api";
import { Select } from "./Select";
import type { Engine } from "./gen/stackwright/v1/projects_pb";
import { useLoad } from "./useLoad";

type Props = { engine: Engine; value: string; onChange: (v: string) => void; label?: string };

// EngineVersion offers the engine's released versions, newest first, and
// picks the newest when there is no value yet. If the release list cannot
// load, it takes a typed version.
export function EngineVersion({ engine, value, onChange, label = "Version" }: Props) {
  const list = useLoad(`engine-versions:${engine}`, () => projectApi.listEngineVersions({ engine }).then((r) => r.versions));
  const loaded = list.data;
  useEffect(() => {
    if (!value && loaded?.length) onChange(loaded[0]);
  }, [value, loaded, onChange]);
  const versions = loaded ?? [];

  if (list.error) {
    return (
      <label className="field">
        <span>{label}</span>
        <input className="mono" required value={value} onChange={(e) => onChange(e.target.value)} placeholder="1.10.7" />
        <span className="hint">The release list did not load; type a version.</span>
      </label>
    );
  }
  return (
    <label className="field">
      <span>{label}</span>
      <Select
        className="mono"
        value={list.data ? value : ""}
        onChange={onChange}
        disabled={!list.data}
        options={[
          ...(!list.data ? [{ value: "", label: "Loading…" }] : []),
          ...(value && !versions.includes(value) ? [{ value, label: value }] : []),
          ...versions.map((v, i) => ({ value: v, label: i === 0 ? `${v} (newest)` : v, text: v })),
        ]}
      />
    </label>
  );
}
