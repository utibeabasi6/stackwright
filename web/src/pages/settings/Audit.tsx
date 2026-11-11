import { useState } from "react";
import { useParams } from "@tanstack/react-router";
import { message, orgApi } from "../../api";
import type { AuditEvent } from "../../gen/stackwright/v1/orgs_pb";
import { useLoad } from "../../useLoad";
import { timestampDate } from "@bufbuild/protobuf/wkt";

export function Audit() {
  const { org } = useParams({ strict: false }) as { org: string };
  const first = useLoad(`audit:${org}`, () => orgApi.listAuditEvents({ org }));
  const [more, setMore] = useState<AuditEvent[]>([]);
  const [error, setError] = useState<string>();
  const events = [...(first.data?.events ?? []), ...more];

  const older = async () => {
    try {
      const last = events[events.length - 1];
      const r = await orgApi.listAuditEvents({ org, beforeId: last.id });
      setMore((m) => [...m, ...r.events]);
    } catch (err) {
      setError(message(err));
    }
  };

  return (
    <div className="stack">
      {(first.error || error) && <p className="error">{first.error || error}</p>}
      <div className="table-box">
        <table className="table">
          <thead>
            <tr>
              <th>When</th>
              <th>Who</th>
              <th>Action</th>
              <th>Target</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e) => (
              <tr key={String(e.id)}>
                <td>{e.at ? timestampDate(e.at).toLocaleString() : ""}</td>
                <td>{e.actor}</td>
                <td className="mono">{e.action}</td>
                <td className="mono">{e.target}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {events.length > 0 && events.length % 50 === 0 && (
        <button type="button" onClick={older}>
          Older
        </button>
      )}
    </div>
  );
}
