import { useState } from "react";
import { ActionButton, Submit, useAction } from "../../Action";
import { useParams } from "@tanstack/react-router";
import { agentApi, message } from "../../api";
import { Role } from "../../gen/stackwright/v1/orgs_pb";
import { useRole } from "../../me";
import { useLoad } from "../../useLoad";

const dollars = (n: number) => `$${n.toFixed(2)}`;

// AgentSettings holds the org's OpenRouter key, its two models and its
// monthly spending cap.
export function AgentSettings() {
  const { org } = useParams({ strict: false }) as { org: string };
  const admin = useRole(org) >= Role.ADMIN;
  const settings = useLoad(`agent-settings:${org}`, () => agentApi.getAgentSettings({ org }).then((r) => r.settings));
  const [error, setError] = useState<string>();
  const saving = useAction();
  const removing = useAction();

  const save = async (form: HTMLFormElement, clearKey = false) => {
    const f = new FormData(form);
    setError(undefined);
    try {
      await agentApi.setAgentSettings({
        org,
        apiKey: String(f.get("apiKey") ?? ""),
        clearKey,
        turnModel: String(f.get("turnModel")),
        shortModel: String(f.get("shortModel")),
        monthlyCap: Number(f.get("monthlyCap")),
      });
      form.reset();
      settings.reload();
      return true;
    } catch (err) {
      setError(message(err));
      return false;
    }
  };

  const s = settings.data;
  if (settings.error) return <p className="error">{settings.error}</p>;
  if (!s) return null;
  return (
    <section className="card stack" aria-labelledby="agent-h" style={{ maxWidth: 640 }}>
      <h2 id="agent-h">Agent</h2>
      <p className="muted">
        The agent calls models through OpenRouter, with this org's key. It stops when the month's spend reaches the cap; plans and applies
        keep running.
      </p>
      <dl className="facts">
        <dt>OpenRouter key</dt>
        <dd>{s.hasKey ? "Set. It is sealed and never shown again." : "Not set. The agent is off."}</dd>
        <dt>Spent this month</dt>
        <dd>
          {dollars(s.spentThisMonth)} of {dollars(s.monthlyCap)}
        </dd>
      </dl>
      {error && <p className="error">{error}</p>}
      {admin && (
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            const form = e.currentTarget;
            void saving.run(() => save(form));
          }}
          key={`${s.turnModel}|${s.shortModel}|${s.monthlyCap}`}
        >
          <label className="field">
            <span>{s.hasKey ? "Replace the key (optional)" : "OpenRouter key"}</span>
            <input name="apiKey" type="password" autoComplete="off" className="mono" placeholder="sk-or-…" />
          </label>
          <div className="row">
            <label className="field grow">
              <span>Model for turns</span>
              <input name="turnModel" required className="mono" defaultValue={s.turnModel} />
            </label>
            <label className="field grow">
              <span>Model for short tasks</span>
              <input name="shortModel" required className="mono" defaultValue={s.shortModel} />
            </label>
          </div>
          <label className="field" style={{ maxWidth: 200 }}>
            <span>Monthly cap in US dollars</span>
            <input name="monthlyCap" type="number" min={0} step="0.01" required defaultValue={s.monthlyCap} />
          </label>
          <div className="row">
            <Submit action={saving} className="primary" busy="Saving…" done="Saved">
              Save
            </Submit>
            {s.hasKey && (
              <ActionButton
                action={removing}
                type="button"
                className="danger"
                onClick={(e) => {
                  const form = e.currentTarget.form!;
                  void removing.run(() => save(form, true));
                }}
                busy="Removing…"
                done="Removed"
              >
                Remove the key
              </ActionButton>
            )}
          </div>
        </form>
      )}
    </section>
  );
}
