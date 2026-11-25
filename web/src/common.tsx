import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { request, type AppInfo } from "./api";

export const RefreshContext = createContext({ auto: false, revision: 0, refresh: () => {} });

export function useResource<T>(path: string | undefined) {
  const { auto, revision } = useContext(RefreshContext);
  const [result, setResult] = useState<{ path: string; data?: T; error?: string }>();
  const reload = useRef<(() => void) | undefined>(undefined);

  useEffect(() => {
    if (!path) return;
    const controller = new AbortController();
    let pending = false;
    const load = async () => {
      if (pending) return;
      pending = true;
      try {
        const data = await request<T>(path, { signal: controller.signal });
        if (!controller.signal.aborted) setResult({ path, data });
      } catch (error) {
        if (!controller.signal.aborted) {
          setResult((previous) => ({ path, error: errorText(error), data: previous?.path === path ? previous.data : undefined }));
        }
      } finally {
        pending = false;
      }
    };
    reload.current = () => void load();
    void load();
    return () => {
      reload.current = undefined;
      controller.abort();
    };
  }, [path, revision]);

  useEffect(() => {
    if (!auto || !path) return;
    const id = window.setInterval(() => reload.current?.(), 2000);
    return () => window.clearInterval(id);
  }, [auto, path]);

  return result?.path === path ? result : undefined;
}

export function useOperation() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const inFlight = useRef(false);

  async function run(operation: () => Promise<void>) {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError("");
    try {
      await operation();
    } catch (err) {
      setError(errorText(err));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  return { busy, error, run };
}

export function errorText(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

export function formatTime(value: string | null, milliseconds = false) {
  if (!value) return "—";
  const date = new Date(milliseconds ? Number(value) : value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export function Badge({ children, tone = "neutral" }: { children: ReactNode; tone?: string }) {
  return <span className={`badge tone-${tone}`}>{children}</span>;
}

export function Notice({ children, tone = "info" }: { children: ReactNode; tone?: string }) {
  return <div className={`notice tone-${tone}`} role={tone === "error" ? "alert" : "status"}>{children}</div>;
}

export function Json({ value }: { value: unknown }) {
  return <pre className="json">{value == null ? "—" : JSON.stringify(value, null, 2)}</pre>;
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return <div className="empty"><strong>{title}</strong>{children && <p>{children}</p>}</div>;
}

export function AppOptions({ apps, all }: { apps: AppInfo[]; all?: string }) {
  return <>{all && <option value="">{all}</option>}
    {apps.map((app) => <option key={app.app_id} value={app.app_id}>{app.bot_name || app.app_id}{app.bot_name && ` · ${app.app_id}`}</option>)}</>;
}
