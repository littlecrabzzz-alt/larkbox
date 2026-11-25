import { useContext, useEffect, useRef, useState, type MouseEvent } from "react";
import { createRoot } from "react-dom/client";
import { resetSandbox, sandbox, type Health, type Snapshot } from "./api";
import ChatPlayground from "./Chat";
import { Badge, Empty, Notice, RefreshContext, useOperation, useResource } from "./common";
import { CardKit, ProtocolTraces } from "./Inspectors";
import Tools from "./Tools";
import "./styles.css";

const tabs = [
  { path: "/chats", label: "Chat Playground", symbol: "▤", description: "Send virtual messages and test bot interactions." },
  { path: "/cards", label: "CardKit", symbol: "▣", description: "Inspect card entities, streaming updates, and addressable elements." },
  { path: "/traces", label: "Protocol Traces", symbol: "⇄", description: "Inspect inbound Open API calls and outbound event deliveries." },
  { path: "/faults", label: "Chaos & Tools", symbol: "◇", description: "Simulate failures and inspect sandbox connections." },
] as const;

function currentPath() {
  return window.location.pathname.replace(/\/$/, "") || "/";
}

function App() {
  const [auto, setAuto] = useState(true);
  const [revision, setRevision] = useState(0);
  return <RefreshContext.Provider value={{ auto, revision, refresh: () => setRevision((value) => value + 1) }}>
    <Inspector setAuto={setAuto} />
  </RefreshContext.Provider>;
}

function Inspector({ setAuto }: { setAuto: (auto: boolean) => void }) {
  const { auto, refresh } = useContext(RefreshContext);
  const [epoch, setEpoch] = useState(0);
  const state = useResource<Snapshot>(sandbox("/state"), epoch);
  const health = useResource<Health>("/healthz");
  const [path, setPath] = useState(currentPath);
  const [confirm, setConfirm] = useState(false);
  const [resetDone, setResetDone] = useState(false);
  const reset = useOperation();
  const cancelReset = useRef<HTMLButtonElement>(null);
  const resetButton = useRef<HTMLButtonElement>(null);
  const active = tabs.find((tab) => tab.path === path) ?? tabs[0];

  useEffect(() => {
    const update = () => setPath(currentPath());
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  }, []);

  useEffect(() => {
    if (confirm) cancelReset.current?.focus();
  }, [confirm]);

  function closeConfirmation() {
    setConfirm(false);
    resetButton.current?.focus();
  }

  function navigate(event: MouseEvent<HTMLAnchorElement>, next: string) {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    if (path === next) return;
    window.history.pushState(null, "", next);
    setPath(next);
  }

  const connected = state?.data?.ws_sessions.length;
  const strict = state?.data?.strict_mode ?? health?.data?.strict_mode;
  const online = health?.data?.status === "ok" && !health.error;
  const healthText = online ? "Online" : health ? "Unavailable" : "Connecting";

  return <>
    <a className="skip-link" href="#main">Skip to workspace</a>
    <header className="topbar">
      <div className="shell topbar-inner">
        <a className="brand" href="/" onClick={(event) => navigate(event, "/")}>
          <span className="brand-mark" aria-hidden="true"><i /><i /><i /></span>
          <span><strong>larkbox</strong><small>Web Inspector</small></span>
        </a>
        <div className="server-status" aria-label="Server status">
          <Badge tone={online ? "success" : health ? "error" : "neutral"}><i className="status-dot" />{healthText}</Badge>
          {strict !== undefined && <Badge>{strict ? "Strict mode" : "Permissive mode"}</Badge>}
          <Badge tone={connected ? "info" : "neutral"}>{connected ?? "—"} WS sessions</Badge>
        </div>
        <div className="global-actions">
          <label className="check auto-toggle"><input type="checkbox" checked={auto} onChange={(event) => setAuto(event.target.checked)} /> Auto-refresh</label>
          <button type="button" onClick={refresh}>Refresh</button>
          <button className="danger" type="button" ref={resetButton} onClick={() => { setConfirm(true); setResetDone(false); }}
            disabled={reset.busy} aria-expanded={confirm}>Reset sandbox</button>
        </div>
      </div>
    </header>
    <nav className="navigation" aria-label="Inspector">
      <div className="shell nav-inner">{tabs.map((tab) => <a href={tab.path} key={tab.path}
        onClick={(event) => navigate(event, tab.path)} aria-current={active.path === tab.path ? "page" : undefined}>
        <span aria-hidden="true">{tab.symbol}</span>{tab.label}
      </a>)}</div>
    </nav>

    <main className="shell main" id="main">
      {confirm && <div className="confirm">
        <div><strong>Reset the sandbox?</strong><p>This restores the startup seed, clears runtime state, and disconnects WebSocket sessions.</p></div>
        <div className="global-actions">
          <button type="button" ref={cancelReset} disabled={reset.busy} onClick={closeConfirmation}>Cancel</button>
          <button type="button" className="danger" disabled={reset.busy} onClick={() => void reset.run(async () => {
            await resetSandbox();
            setEpoch((value) => value + 1);
            setConfirm(false);
            setResetDone(true);
            refresh();
          })}>{reset.busy ? "Resetting…" : "Confirm reset"}</button>
        </div>
      </div>}
      {confirm && reset.error && <Notice tone="error">{reset.error}</Notice>}
      {resetDone && <Notice tone="success">Sandbox reset to its startup seed.</Notice>}
      {health?.error && <Notice tone="error">{health.error}</Notice>}
      {state?.error && <Notice tone="error">{state.error}</Notice>}
      <div className="page-heading">
        <div><h1>{active.label}</h1><p>{active.description}</p></div>
        {health?.data && <code className="version">{health.data.version}</code>}
      </div>
      <div key={active.path}>
        {state?.data ? <>
          {active.path === "/chats" && <ChatPlayground state={state.data} />}
          {active.path === "/cards" && <CardKit />}
          {active.path === "/traces" && <ProtocolTraces apps={state.data.apps} />}
          {active.path === "/faults" && <Tools apps={state.data.apps} />}
        </> : <section className="panel"><Empty title={state?.error ? "Sandbox unavailable" : "Loading sandbox…"} /></section>}
      </div>
    </main>
  </>;
}

createRoot(document.getElementById("root")!).render(<App />);
