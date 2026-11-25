export type AppInfo = {
  app_id: string;
  bot_open_id: string;
  bot_name: string;
  webhook_url: string;
  active_ws_sessions: number;
};

export type User = { open_id: string; name: string; inactive?: boolean };
export type Chat = { chat_id: string; chat_mode: string; name: string; member_ids: string[] };
export type Session = {
  session_id: string;
  app_id: string;
  device_id: string;
  service_id: number;
  connected_at: string;
};
export type Snapshot = {
  strict_mode: boolean;
  apps: AppInfo[];
  users: User[] | null;
  chats: Chat[];
  ws_sessions: Session[];
};
export type Health = { status: string; strict_mode: boolean; version: string };

export type Message = {
  message_id: string;
  chat_id: string;
  create_time: string;
  msg_type: string;
  content: string;
  sender: { id: string; sender_type: string; sender_name?: string };
  deleted: boolean;
  parent_id?: string;
  root_id?: string;
  thread_id?: string;
  mentions: { key: string; name: string; id: string }[];
  reactions: { reaction_id: string; emoji_type: string; operator_open_id: string; operator_app_id: string }[];
  raw_card_json?: unknown;
  rendered_card_json?: unknown;
  card_id?: string;
};
export type SendInput = {
  chat_id: string;
  sender_open_id: string;
  at_bot: boolean;
  parent_id?: string;
  reply_in_thread?: boolean;
} & ({ text: string } | { msg_type: string; content: string });
export type Delivery = { channel: string; status_code: number; error: string };
export type SendResult = {
  message: Message;
  deliveries: { app_id: string; event_id: string; deliveries: Delivery[]; error: string }[];
};
export type CardAction = {
  tag: string;
  value?: Record<string, unknown>;
  name?: string;
  option?: string;
  options?: string[];
  input_value?: string;
  checked?: boolean;
  timezone?: string;
  form_value?: Record<string, string | string[] | boolean>;
};
export type ActionResult = {
  toast: { type?: string; content?: string; i18n?: Record<string, string> } | null;
  card_applied: boolean;
  card_error: string;
  message: Message | null;
  delivery: Delivery & { event_id: string };
};
export type CardEntity = {
  card_id: string;
  app_id: string;
  type: string;
  streaming_mode: boolean;
  last_sequence: number;
  revision: number;
  created_at: string;
  expires_at: string;
  streaming_expires_at: string | null;
  card_json?: unknown;
  elements?: Record<string, string>;
};
export type Matchers = {
  app_id?: string;
  method?: string;
  path_prefix?: string;
  event_type?: string;
  channel?: string;
};
export type TraceFilters = Matchers & { direction?: string; event_id?: string; limit?: number };
export type Trace = {
  direction: "inbound" | "outbound";
  started_at: string;
  latency_ms: number;
  app_id: string;
  method: string;
  path: string;
  status_code: number;
  code: number | null;
  request_headers: unknown;
  request_body: unknown;
  request_body_truncated: boolean;
  response_headers: unknown;
  response_body: unknown;
  response_body_truncated: boolean;
  event_id: string;
  event_type: string;
  channel: string;
  attempt: number;
  duplicate_index: number;
  error: string;
};
export type FaultKind = "latency" | "rate_limit" | "http_error" | "token_expiry" | "duplicate_event";
export type FaultSpec = Matchers & {
  kind: FaultKind;
  remaining_hits?: number;
  probability?: number;
  delay_ms?: number;
  retry_after?: number;
  status_code?: number;
  error_code?: number;
  error_msg?: string;
  duplicate_count?: number;
};
export type Fault = FaultSpec & { fault_id: string };
export type Verification = { verified: boolean; status_code: number; latency_ms: number; error: string };

export function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    ...options,
    headers: {
      Accept: "application/json",
      ...(options.body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    cache: "no-store",
    mode: "same-origin",
  });
  const body: unknown = await response.json().catch(() => undefined);
  if (!response.ok) {
    const error = record(body)?.error;
    throw new Error(typeof error === "string" ? error : `HTTP ${response.status} from ${path}`);
  }
  if (body === undefined) throw new Error(`Expected JSON from ${path}`);
  return body as T;
}

export const sandbox = (path: string) => "/_sandbox" + path;
export const messagesPath = (id: string) => sandbox(`/chats/${encodeURIComponent(id)}/messages`);
export const cardPath = (id: string) => sandbox(`/cards/${encodeURIComponent(id)}`);
export const cardImagePath = (key: string) => sandbox(`/images/${encodeURIComponent(key)}`);
export const faultPath = (id: string) => sandbox(`/faults/${encodeURIComponent(id)}`);

export const MAX_TRACE_LIMIT = 256;

export function tracesPath({ limit, ...filters }: TraceFilters) {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filters)) {
    if (value?.trim()) query.set(key, value.trim());
  }
  if (limit !== undefined && Number.isFinite(limit)) {
    query.set("limit", String(Math.max(1, Math.min(MAX_TRACE_LIMIT, Math.trunc(limit)))));
  }
  return sandbox("/traces") + (query.size ? "?" + query.toString() : "");
}

const post = <T>(path: string, payload: unknown) =>
  request<T>(sandbox(path), { method: "POST", body: JSON.stringify(payload) });

export const sendMessage = (input: SendInput) => post<SendResult>("/messages/send", input);
export const triggerCard = (message_id: string, operator_open_id: string, action: CardAction) =>
  post<ActionResult>("/cards/action", { message_id, operator_open_id, action });
export const validateCard = (card: unknown) => post<{ valid: boolean; error?: string }>("/cards/validate", { card });
export const resetSandbox = () => post<{ reset: boolean }>("/reset", {});
export const addFault = (input: FaultSpec) => post<Fault>("/faults", input);
export const verifyWebhook = (app_id: string, webhook_url?: string) =>
  post<Verification>("/events/webhook/verify", { app_id, webhook_url });
export const expireTokens = (app_id: string) => post<{ expired: number }>("/tokens/expire", { app_id });
export const remove = (path: string) => request(path, { method: "DELETE" });
