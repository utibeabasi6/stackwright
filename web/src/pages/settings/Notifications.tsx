import { useState, type FormEvent } from "react";
import { useParams } from "@tanstack/react-router";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Action, Submit, useAction } from "../../Action";
import { cloudApi, message, notificationApi } from "../../api";
import { Copy } from "../../Copy";
import { DestinationKind, NotificationEvent } from "../../gen/stackwright/v1/notifications_pb";
import { Select } from "../../Select";
import { useLoad } from "../../useLoad";

const eventNames: [NotificationEvent, string][] = [
  [NotificationEvent.RUN_FAILED, "A run failed"],
  [NotificationEvent.RUN_NEEDS_APPROVAL, "A run waits for approval"],
  [NotificationEvent.RUN_APPLIED, "A run applied"],
  [NotificationEvent.FINDING_OPENED, "A High or Critical finding opened"],
  [NotificationEvent.DRIFT_FOUND, "Drift found"],
];

// Notifications sets up Slack and webhook destinations, and the audit
// export (adr/0045).
export function Notifications() {
  const { org } = useParams({ strict: false }) as { org: string };
  const list = useLoad(`destinations:${org}`, () => notificationApi.listDestinations({ org }).then((r) => r.destinations));
  const audit = useLoad(`audit-export:${org}`, () => notificationApi.getAuditExport({ org }));
  const accounts = useLoad(`cloud:${org}`, () => cloudApi.listCloudAccounts({ org }));
  const [kind, setKind] = useState(DestinationKind.SLACK);
  const [events, setEvents] = useState<NotificationEvent[]>([NotificationEvent.RUN_FAILED]);
  const [secret, setSecret] = useState<string>();
  const [auditSecret, setAuditSecret] = useState<string>();
  const [account, setAccount] = useState<string>();
  const [error, setError] = useState<string>();
  const adding = useAction();
  const savingAudit = useAction();
  const fail = (err: unknown) => {
    setError(message(err));
    throw err;
  };

  const add = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const form = e.currentTarget;
    const f = new FormData(form);
    setError(undefined);
    void adding.run(async () => {
      const r = await notificationApi
        .createDestination({ org, kind, name: String(f.get("name")), url: String(f.get("url")), events })
        .catch(fail);
      setSecret(r.secret || undefined);
      form.reset();
      list.reload();
    });
  };
  const saveAudit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    setError(undefined);
    void savingAudit.run(async () => {
      const r = await notificationApi
        .saveAuditExport({
          org,
          retentionDays: Number(f.get("retention")),
          s3Bucket: String(f.get("bucket")),
          s3Prefix: String(f.get("prefix")),
          s3CloudAccount: account ?? audit.data?.s3CloudAccount ?? "",
          webhookUrl: String(f.get("webhook")),
          stopWebhook: f.get("stopWebhook") === "on",
        })
        .catch(fail);
      setAuditSecret(r.webhookSecret || undefined);
      audit.reload();
    });
  };
  const cursor = (t?: { sentAt?: Parameters<typeof timestampDate>[0]; lastError: string }) =>
    t?.lastError ? `! ${t.lastError}` : t?.sentAt ? `Sent up to ${timestampDate(t.sentAt).toLocaleString()}` : "Nothing sent yet";

  return (
    <div className="stack">
      {error && <p className="error">{error}</p>}
      <section className="stack" aria-labelledby="dest-h">
        <h2 id="dest-h">Notifications</h2>
        {list.error && <p className="error">{list.error}</p>}
        {list.data?.length === 0 && <p className="muted">No destinations yet.</p>}
        {(list.data?.length ?? 0) > 0 && (
          <div className="table-box">
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Sends</th>
                  <th>Last delivery</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {list.data?.map((d) => (
                  <tr key={d.id}>
                    <td>
                      {d.name}
                      <span className="muted"> · {d.kind === DestinationKind.SLACK ? "Slack" : "Webhook"} · {d.host}</span>
                    </td>
                    <td>
                      <div className="stack">
                        {eventNames.map(([e, n]) => (
                          <label key={e}>
                            <input
                              type="checkbox"
                              checked={d.events.includes(e)}
                              onChange={(ev) =>
                                void notificationApi
                                  .updateDestinationEvents({ org, id: d.id, events: ev.target.checked ? [...d.events, e] : d.events.filter((x) => x !== e) })
                                  .then(() => list.reload(), fail)
                              }
                            />{" "}
                            {n}
                          </label>
                        ))}
                      </div>
                    </td>
                    <td>
                      {d.lastError ? (
                        <span className="error">! {d.lastError}</span>
                      ) : d.lastDeliveredAt ? (
                        timestampDate(d.lastDeliveredAt).toLocaleString()
                      ) : (
                        <span className="muted">None yet</span>
                      )}
                    </td>
                    <td className="row">
                      <Action run={() => notificationApi.testDestination({ org, id: d.id }).catch(fail)} busy="Sending…" done="Sent">
                        Send a test
                      </Action>
                      <Action className="danger" confirm={{ message: `Delete ${d.name}?`, action: "Delete", danger: true }}
                        run={() => notificationApi.deleteDestination({ org, id: d.id }).then(() => list.reload(), fail)} busy="Deleting…" done="Deleted">
                        Delete
                      </Action>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {secret && <Copy label="The webhook's signing secret, shown once" text={secret} />}
        <form className="card stack" onSubmit={add} aria-label="New destination">
          <div className="row">
            <label className="field">
              <span>Kind</span>
              <Select aria-label="Kind" value={String(kind)} onChange={(v) => setKind(Number(v) as DestinationKind)}
                options={[{ value: String(DestinationKind.SLACK), label: "Slack" }, { value: String(DestinationKind.WEBHOOK), label: "Webhook" }]} />
            </label>
            <label className="field">
              <span>Name</span>
              <input name="name" required maxLength={100} placeholder="#platform-alerts" />
            </label>
          </div>
          <label className="field">
            <span>{kind === DestinationKind.SLACK ? "Slack incoming webhook URL" : "Webhook URL"}</span>
            <input name="url" type="url" required className="mono" placeholder="https://hooks.slack.com/services/…" />
            {kind === DestinationKind.WEBHOOK && (
              <span className="hint">Each post carries X-Stackwright-Signature: sha256= HMAC of the body with the secret.</span>
            )}
          </label>
          <fieldset className="stack">
            <legend>Send when</legend>
            {eventNames.map(([e, name]) => (
              <label key={e}>
                <input type="checkbox" checked={events.includes(e)}
                  onChange={(x) => setEvents(x.target.checked ? [...events, e] : events.filter((y) => y !== e))} /> {name}
              </label>
            ))}
          </fieldset>
          <div>
            <Submit action={adding} className="primary" busy="Adding…" done="Added" disabled={events.length === 0}>
              Add destination
            </Submit>
          </div>
        </form>
      </section>

      <section className="stack" aria-labelledby="audit-h">
        <h2 id="audit-h">Audit export</h2>
        {audit.data && (
          <form className="card stack" onSubmit={saveAudit} aria-label="Audit export" key={audit.data.s3Bucket + audit.data.webhookHost}>
            <label className="field">
              <span>Keep the audit log for (days)</span>
              <input name="retention" type="number" min={30} max={3650} required defaultValue={audit.data.retentionDays} />
              <span className="hint">Older rows go once every export has them.</span>
            </label>
            <h3>S3</h3>
            <div className="row">
              <label className="field">
                <span>Bucket</span>
                <input name="bucket" className="mono" defaultValue={audit.data.s3Bucket} placeholder="empty stops the export" />
              </label>
              <label className="field">
                <span>Prefix</span>
                <input name="prefix" className="mono" defaultValue={audit.data.s3Prefix} />
              </label>
              <label className="field">
                <span>Cloud account</span>
                <Select aria-label="Cloud account" value={account ?? audit.data.s3CloudAccount} onChange={setAccount}
                  options={[{ value: "", label: "Choose one" }, ...(accounts.data?.accounts ?? []).map((a) => ({ value: a.name, label: a.name }))]} />
              </label>
            </div>
            <p className="hint">
              The export writes with the account&apos;s apply role. Its trust policy must allow the subject{" "}
              <span className="mono">org:&lt;org id&gt;:workspace::phase:audit-export</span>.
            </p>
            {audit.data.s3Bucket && <p className="muted">{cursor(audit.data.s3)}</p>}
            <h3>Webhook</h3>
            <label className="field">
              <span>URL</span>
              <input name="webhook" type="url" className="mono" placeholder={audit.data.webhookHost ? "empty keeps it as it is" : "https://…"} />
              <span className="hint">A new URL gets a new signing secret.</span>
            </label>
            {audit.data.webhookHost && (
              <>
                <p className="muted">
                  Sends to <span className="mono">{audit.data.webhookHost}</span>. {cursor(audit.data.webhook)}
                </p>
                <label>
                  <input type="checkbox" name="stopWebhook" /> Stop the webhook export
                </label>
              </>
            )}
            {auditSecret && <Copy label="The audit webhook's signing secret, shown once" text={auditSecret} />}
            <div>
              <Submit action={savingAudit} className="primary" busy="Saving…" done="Saved">
                Save
              </Submit>
            </div>
          </form>
        )}
      </section>
    </div>
  );
}
