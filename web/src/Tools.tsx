import { useContext, useState, type FormEvent } from "react";
import {
  addFault, expireTokens, faultPath, remove, sandbox, verifyWebhook,
  type AppInfo, type Fault, type FaultKind, type FaultSpec, type Session, type Verification,
} from "./api";
import { AppOptions, Badge, Empty, formatTime, Json, Notice, RefreshContext, useOperation, useResource } from "./common";

const kinds: Record<FaultKind, string> = {
  latency: "Latency", rate_limit: "Rate limit", http_error: "HTTP error",
  token_expiry: "Token expiry", duplicate_event: "Duplicate event",
};

export default function Tools({ apps }: { apps: AppInfo[] }) {
  return <div className="stack">
    <div className="two-columns"><ActiveFaults /><CreateFault apps={apps} /></div>
    <Sessions />
    <Utilities apps={apps} />
  </div>;
}

function ActiveFaults() {
  const faults = useResource<{ faults: Fault[] }>(sandbox("/faults"));
  const { refresh } = useContext(RefreshContext);
  const operation = useOperation();
  const list = faults?.data?.faults;
  function deleteRule(path: string) {
    void operation.run(async () => {
      await remove(path);
      refresh();
    });
  }

  return <section className="panel">
    <div className="panel-heading"><h2>Active fault rules <Badge>{list?.length ?? "—"}</Badge></h2>
      <button className="danger" type="button" disabled={operation.busy || !list?.length}
        onClick={() => deleteRule(sandbox("/faults"))}>Clear all</button>
    </div>
    {faults?.error && <Notice tone="error">{faults.error}</Notice>}
    {operation.error && <Notice tone="error">{operation.error}</Notice>}
    {!faults && <Empty title="Loading rules…" />}
    {list?.length === 0 && <Empty title="No active fault rules">Create a rule to simulate a protocol failure or delay.</Empty>}
    <ul className="rules">{list?.map((fault) => <li key={fault.fault_id}>
      <div className="panel-heading"><div>
        <strong>{kinds[fault.kind]}</strong> <Badge>{fault.remaining_hits ?? "Unlimited"}{fault.remaining_hits != null && " hits left"}</Badge>
        <div><code>{fault.fault_id}</code></div>
      </div><button type="button" disabled={operation.busy} className="danger"
        onClick={() => deleteRule(faultPath(fault.fault_id))}>Delete</button></div>
      <details><summary>Rule JSON</summary><Json value={fault} /></details>
    </li>)}</ul>
  </section>;
}

function CreateFault({ apps }: { apps: AppInfo[] }) {
  const [kind, setKind] = useState<FaultKind>("latency");
  const [created, setCreated] = useState("");
  const { refresh } = useContext(RefreshContext);
  const operation = useOperation();
  const inbound = kind !== "latency" && kind !== "duplicate_event";

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const values = new FormData(event.currentTarget);
    const value = (name: string) => String(values.get(name) ?? "").trim() || undefined;
    const number = (name: string) => {
      const raw = value(name);
      return raw === undefined ? undefined : Number(raw);
    };
    const spec: FaultSpec = {
      kind, app_id: value("app_id"), method: value("method")?.toUpperCase(),
      path_prefix: value("path_prefix"), event_type: value("event_type"), channel: value("channel"),
      remaining_hits: number("remaining_hits"), delay_ms: number("delay_ms"),
      status_code: number("status_code"), duplicate_count: number("duplicate_count"),
    };
    void operation.run(async () => {
      setCreated("");
      const result = await addFault(spec);
      setCreated(result.fault_id);
      refresh();
    });
  }

  return <section className="panel">
    <div className="panel-heading"><h2>Create fault rule</h2></div>
    <form className="panel-content form-grid" onSubmit={submit}>
      <label>Kind<select value={kind} onChange={(event) => setKind(event.target.value as FaultKind)}>
        {Object.entries(kinds).map(([value, label]) => <option value={value} key={value}>{label}</option>)}
      </select></label>
      <label>App<select name="app_id"><AppOptions apps={apps} all="Any app" /></select></label>
      <label>Remaining hits<input name="remaining_hits" type="number" min="1" placeholder="Unlimited" /></label>
      <label>Channel<select key={kind} name="channel">
        <option value="">{inbound ? "Any inbound request" : kind === "duplicate_event" ? "Any outbound channel" : "Any channel"}</option>
        {kind !== "duplicate_event" && <option value="inbound">Inbound</option>}
        {!inbound && <><option value="webhook">Webhook</option><option value="websocket">WebSocket</option></>}
      </select></label>
      <label>Method<input name="method" placeholder="Any method" pattern="[A-Za-z]+" /></label>
      <label>Path prefix<input name="path_prefix" placeholder={kind === "duplicate_event" ? "/webhook" : "/open-apis/"} /></label>
      {!inbound && <label>Event type<input name="event_type" placeholder="Any event" /></label>}
      {kind === "latency" && <label>Delay (ms)<input name="delay_ms" type="number" min="1" max="60000" defaultValue="250" required /></label>}
      {kind === "http_error" && <label>Status code<input name="status_code" type="number" min="400" max="599" placeholder="500" /></label>}
      {kind === "duplicate_event" && <label>Total copies<input name="duplicate_count" type="number" min="2" max="10" placeholder="2" /></label>}
      <div className="full-width"><button className="primary" disabled={operation.busy} type="submit">{operation.busy ? "Creating…" : "Create rule"}</button></div>
      {operation.error && <div className="full-width"><Notice tone="error">{operation.error}</Notice></div>}
      {created && <div className="full-width"><Notice tone="success">Created <code>{created}</code></Notice></div>}
    </form>
  </section>;
}

function Sessions() {
  const sessions = useResource<{ ws_sessions: Session[] }>(sandbox("/ws/sessions"));
  const rows = sessions?.data?.ws_sessions;

  return <section className="panel">
    <div className="panel-heading"><h2>WebSocket sessions</h2><Badge tone={rows?.length ? "success" : "neutral"}>{rows?.length ?? "—"} connected</Badge></div>
    {sessions?.error && <Notice tone="error">{sessions.error}</Notice>}
    {!sessions && <Empty title="Loading sessions…" />}
    {rows?.length === 0 && <Empty title="No active connections">Connect a bot using the official SDK long connection.</Empty>}
    {!!rows?.length && <div className="table-wrap"><table>
      <thead><tr><th>Session</th><th>App</th><th>Device</th><th>Service</th><th>Connected</th></tr></thead>
      <tbody>{rows.map((row) => <tr key={row.session_id}>
        <td><code>{row.session_id}</code></td><td><code>{row.app_id}</code></td><td><code>{row.device_id}</code></td>
        <td>{row.service_id}</td><td>{formatTime(row.connected_at)}</td>
      </tr>)}</tbody>
    </table></div>}
  </section>;
}

function Utilities({ apps }: { apps: AppInfo[] }) {
  const [selected, setSelected] = useState("");
  const app = apps.find((item) => item.app_id === selected) ?? apps[0];
  const [url, setURL] = useState("");
  const [verified, setVerified] = useState<{ appID: string; value: Verification }>();
  const [expired, setExpired] = useState<{ appID: string; count: number }>();
  const verify = useOperation();
  const expire = useOperation();

  return <section className="panel">
    <div className="panel-heading"><h2>Sandbox utilities</h2></div>
    <div className="panel-content stack">
      <label className="app-control">Target app
        <select value={app?.app_id ?? ""} disabled={!app || verify.busy || expire.busy} onChange={(event) => setSelected(event.target.value)}>
          <AppOptions apps={apps} all={app ? undefined : "No apps configured"} />
        </select>
      </label>
      <div className="two-columns utilities">
        <form className="stack" onSubmit={(event) => {
          event.preventDefault();
          if (!app) return;
          void verify.run(async () => {
            setVerified(undefined);
            setVerified({ appID: app.app_id, value: await verifyWebhook(app.app_id, url.trim() || undefined) });
          });
        }}>
          <div><h3>Webhook verification</h3><p className="muted">Send a <code>url_verification</code> challenge.</p></div>
          <label>Webhook URL override<input type="url" value={url} onChange={(event) => setURL(event.target.value)}
            placeholder={app?.webhook_url || "Use configured URL"} /></label>
          <div><button type="submit" disabled={!app || verify.busy}>{verify.busy ? "Verifying…" : "Verify webhook"}</button></div>
          {verify.error && <Notice tone="error">{verify.error}</Notice>}
          {verified?.appID === app?.app_id && verified && <Notice tone={verified.value.verified ? "success" : "error"}>
            {verified.value.verified ? "Verified" : "Verification failed"} · Status {verified.value.status_code} · {verified.value.latency_ms} ms
            {verified.value.error && <div>{verified.value.error}</div>}
          </Notice>}
        </form>
        <div className="stack">
          <div><h3>Token expiry</h3><p className="muted">Expire all current tokens for the selected app.</p></div>
          <div><button className="danger" type="button" disabled={!app || expire.busy} onClick={() => {
            if (!app) return;
            void expire.run(async () => {
              setExpired(undefined);
              const result = await expireTokens(app.app_id);
              setExpired({ appID: app.app_id, count: result.expired });
            });
          }}>{expire.busy ? "Expiring…" : "Force expire tokens"}</button></div>
          {expire.error && <Notice tone="error">{expire.error}</Notice>}
          {expired?.appID === app?.app_id && expired && <Notice tone="success">
            Expired {expired.count} {expired.count === 1 ? "token" : "tokens"}.
          </Notice>}
        </div>
      </div>
    </div>
  </section>;
}
