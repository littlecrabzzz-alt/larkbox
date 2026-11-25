import { useContext, useState, type FormEvent } from "react";
import {
  cardPath, MAX_TRACE_LIMIT, remove, sandbox, tracesPath, validateCard, type AppInfo, type CardEntity, type Trace, type TraceFilters,
} from "./api";
import { CardPreview, extractCard } from "./CardPreview";
import { AppOptions, Badge, Empty, formatTime, Json, Notice, RefreshContext, useOperation, useResource } from "./common";

function StreamBadge({ card }: { card: CardEntity }) {
  const live = card.streaming_mode && (!card.streaming_expires_at || Date.parse(card.streaming_expires_at) > Date.now())
    && Date.parse(card.expires_at) > Date.now();
  return <Badge tone={live ? "success" : card.streaming_mode ? "warning" : "neutral"}>
    {live ? "Streaming" : card.streaming_mode ? "Stream expired" : "Stream off"}
  </Badge>;
}

export function CardKit() {
  const [cardID, select] = useState("");
  const cards = useResource<{ cards: CardEntity[] }>(sandbox("/cards"));
  const list = cards?.data?.cards;
  const selected = list?.find((card) => card.card_id === cardID) ?? list?.[0];
  const detail = useResource<CardEntity>(selected && cardPath(selected.card_id));
  const card = detail?.data;

  return <div className="workspace">
    <aside className="panel sidebar">
      <div className="panel-heading"><h2>Card entities</h2><Badge>{list?.length ?? "—"}</Badge></div>
      {cards?.error && <Notice tone="error">{cards.error}</Notice>}
      {!cards && <Empty title="Loading cards…" />}
      {list?.length === 0 && <Empty title="No card entities">Create one with the CardKit Open API.</Empty>}
      <div className="item-list">{list?.map((item) => <button type="button" key={item.card_id} className="list-item"
        aria-pressed={selected?.card_id === item.card_id} onClick={() => select(item.card_id)}>
        <span className="item-label"><code>{item.card_id}</code><span className="item-meta">
          <StreamBadge card={item} /> Revision {item.revision}
        </span></span>
      </button>)}</div>
    </aside>
    <section className="panel">
      <div className="panel-heading"><h2>Card details</h2>{card && <StreamBadge card={card} />}</div>
      {detail?.error && <Notice tone="error">{detail.error}</Notice>}
      {!card && !detail?.error && <Empty title={selected ? "Loading card…" : "No card selected"} />}
      {card && <div className="panel-content">
        <dl className="metadata">
          <div><dt>Card ID</dt><dd><code>{card.card_id}</code></dd></div>
          <div><dt>App ID</dt><dd><code>{card.app_id}</code></dd></div>
          <div><dt>Last sequence</dt><dd>{card.last_sequence}</dd></div>
          <div><dt>Revision</dt><dd>{card.revision}</dd></div>
          <div><dt>Created</dt><dd>{formatTime(card.created_at)}</dd></div>
          <div><dt>Entity expires</dt><dd>{formatTime(card.expires_at)}</dd></div>
          <div><dt>Stream expires</dt><dd>{formatTime(card.streaming_expires_at)}</dd></div>
        </dl>
        <h3>Preview</h3>
        <CardPreview card={extractCard(card.card_json)} />
        <h3>Addressable elements</h3>
        <Json value={card.elements} />
        <h3>Raw card JSON</h3>
        <Json value={card.card_json} />
      </div>}
      <CardJSONCheck />
    </section>
  </div>;
}

function CardJSONCheck() {
  const [source, setSource] = useState("");
  const [preview, setPreview] = useState<ReturnType<typeof extractCard>>();
  const operation = useOperation();

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setPreview(undefined);
    void operation.run(async () => {
      let card: unknown;
      try {
        card = JSON.parse(source);
      } catch (error) {
        throw new Error("Invalid JSON. " + (error instanceof Error ? error.message : ""));
      }
      await validateCard(card);
      setPreview(extractCard(card));
    });
  }

  return <details className="panel-content card-panel">
    <summary>Validate card JSON</summary>
    <form className="stack" onSubmit={submit}>
      <label>Card JSON<textarea className="card-json-input" value={source} rows={8}
        onChange={(event) => { setSource(event.target.value); setPreview(undefined); }}
        spellCheck={false} disabled={operation.busy} required /></label>
      <button type="submit" className="primary" disabled={operation.busy || !source.trim()}>Validate and preview</button>
      {operation.error && <Notice tone="error">{operation.error}</Notice>}
      {preview && <>
        <Notice tone="success">Card JSON passed validation.</Notice>
        <CardPreview card={preview} />
      </>}
    </form>
  </details>;
}

function traceKey(trace: Trace) {
  return JSON.stringify([trace.started_at, trace.direction, trace.app_id, trace.method, trace.path,
    trace.event_id, trace.channel, trace.attempt, trace.duplicate_index]);
}
const traceName = (trace: Trace) => trace.direction === "inbound" ? `${trace.method} ${trace.path}` : trace.event_type;

export function ProtocolTraces({ apps }: { apps: AppInfo[] }) {
  const [filters, setFilters] = useState<TraceFilters>({});
  const [selectedKey, select] = useState("");
  const traces = useResource<{ traces: Trace[] }>(tracesPath(filters));
  const rows = traces?.data?.traces;
  const active = rows?.find((trace) => traceKey(trace) === selectedKey) ?? rows?.at(-1);
  const { refresh } = useContext(RefreshContext);
  const operation = useOperation();

  function apply(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const values = new FormData(event.currentTarget);
    const limit = String(values.get("limit") ?? "");
    setFilters({
      direction: String(values.get("direction")),
      app_id: String(values.get("app_id")),
      method: String(values.get("method")).toUpperCase(),
      path_prefix: String(values.get("path_prefix")),
      event_type: String(values.get("event_type")),
      event_id: String(values.get("event_id")),
      channel: String(values.get("channel")),
      limit: limit === "" ? undefined : Number(limit),
    });
    select("");
  }

  return <div className="stack">
    <section className="panel">
      <div className="panel-heading"><h2>Trace filters</h2>
        <button className="danger" type="button" disabled={operation.busy} onClick={() => void operation.run(async () => {
          await remove(sandbox("/traces"));
          select("");
          refresh();
        })}>Clear traces</button>
      </div>
      <form className="filter-grid panel-content" onSubmit={apply}>
        <label>Direction<select name="direction">
          <option value="all">All directions</option><option value="inbound">Inbound Open API</option>
          <option value="outbound">Outbound events</option>
        </select></label>
        <label>App<select name="app_id"><AppOptions apps={apps} all="All apps" /></select></label>
        <label>Method<input name="method" placeholder="GET" /></label>
        <label>Path prefix<input name="path_prefix" placeholder="/open-apis/im/" /></label>
        <label>Event type<input name="event_type" placeholder="im.message.receive_v1" /></label>
        <label>Event ID<input name="event_id" /></label>
        <label>Channel<select name="channel">
          <option value="">All channels</option><option value="webhook">Webhook</option><option value="websocket">WebSocket</option>
        </select></label>
        <label>Limit<input name="limit" type="number" min="1" max={MAX_TRACE_LIMIT} placeholder={String(MAX_TRACE_LIMIT)} /></label>
        <button type="submit" className="primary">Apply filters</button>
      </form>
    </section>
    {operation.error && <Notice tone="error">{operation.error}</Notice>}
    <section className="panel">
      <div className="panel-heading"><h2>Exchanges</h2><Badge>{rows?.length ?? "—"}</Badge></div>
      {traces?.error && <Notice tone="error">{traces.error}</Notice>}
      {!traces && <Empty title="Loading traces…" />}
      {rows?.length === 0 && <Empty title="No matching traces">Make an Open API request or trigger an event to record an exchange.</Empty>}
      {!!rows?.length && <div className="table-wrap"><table>
        <thead><tr><th>Time</th><th>Direction</th><th>Request / event</th><th>Status</th><th>Feishu code</th><th>Latency</th><th>Attempt / copy</th></tr></thead>
        <tbody>{[...rows].reverse().map((trace) => {
          const key = traceKey(trace);
          return <tr key={key} className={active && key === traceKey(active) ? "selected-row" : undefined}>
            <td className="nowrap">{formatTime(trace.started_at)}</td>
            <td><Badge tone={trace.direction === "inbound" ? "info" : "purple"}>{trace.direction === "inbound" ? "Inbound" : "Outbound"}</Badge></td>
            <td><button type="button" className="text-button" aria-pressed={!!active && key === traceKey(active)}
              onClick={() => select(key)}>{traceName(trace)}</button>
              {trace.channel && <small className="muted">{trace.channel}</small>}</td>
            <td>{trace.status_code || "—"}</td>
            <td>{trace.code == null ? "—" : <Badge tone={trace.code === 0 ? "success" : "error"}>{trace.code}</Badge>}</td>
            <td className="nowrap">{trace.latency_ms} ms</td>
            <td>{trace.attempt || "—"} / {trace.direction === "outbound" ? trace.duplicate_index : "—"}</td>
          </tr>;
        })}</tbody>
      </table></div>}
    </section>
    {active && <section className="panel">
      <div className="panel-heading"><h2>Exchange details</h2><Badge>{active.channel || "HTTP"}</Badge></div>
      <div className="panel-content">
        <h3><code>{traceName(active)}</code></h3>
        <dl className="metadata">
          <div><dt>Time</dt><dd>{formatTime(active.started_at)}</dd></div>
          <div><dt>App</dt><dd><code>{active.app_id || "—"}</code></dd></div>
          {active.event_id && <div><dt>Event ID</dt><dd><code>{active.event_id}</code></dd></div>}
        </dl>
        {active.error && <Notice tone="error">{active.error}</Notice>}
        <div className="two-columns">
          <div><h3>Request headers</h3><Json value={active.request_headers} />
            <h3>Request body {active.request_body_truncated && <Badge tone="warning">Truncated</Badge>}</h3>
            <Json value={active.request_body} /></div>
          <div><h3>Response headers</h3><Json value={active.response_headers} />
            <h3>Response body {active.response_body_truncated && <Badge tone="warning">Truncated</Badge>}</h3>
            <Json value={active.response_body} /></div>
        </div>
      </div>
    </section>}
  </div>;
}
