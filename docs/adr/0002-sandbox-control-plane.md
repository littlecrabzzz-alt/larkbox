# 0002. Local sandbox control plane

- **Status:** Accepted
- **Date:** 2026-11-25

## Context

SDK tests need to drive user actions, inspect the resulting state and wire
exchanges, and reproduce delivery failures over HTTP. The embedded Web
Inspector and Chat Playground will use these same operations.

## Decision

Mount the control plane under `/_sandbox/` on the larkbox listener. It uses
JSON request and response objects, including `{"error":"..."}` for request
errors. It accepts a single object per request, rejects unknown fields, and
limits bodies to 1 MiB. Responses carry `Cache-Control: no-store`. The Open
API uses its existing Feishu envelopes. The control plane and `/healthz`
bypass Open API Bearer authentication, auditing, and fault evaluation.

### State and simulation

| Method and path (relative to `/_sandbox`) | Contract |
|---|---|
| `GET /state` | Returns strict mode, build version, sanitized apps, users, chats, card summaries, active faults, and WebSocket sessions. |
| `POST /seed` | Accepts any of `apps`, `users`, `chats`, and `card_templates` in their configuration shape. Upserts whole entries by primary key and templates by ID and version. Returns counts. Validates duplicate IDs, URLs, member and owner references, and card templates before committing. |
| `POST /reset` | Empty body or `{}` restores the configured startup seed. An optional `{"seed":{...}}` replaces the state for this reset, with omitted lists treated as empty. Both forms clear runtime entities, credentials, traces, faults, and WebSocket tickets and disconnect existing sessions. |
| `GET /chats` | Returns `{"chats":[...]}` with member IDs and settings. |
| `GET /chats/{chat_id}/messages` | Returns `{"messages":[...]}` in creation order, including thread replies, sender and mention details, reactions, and available rendered and raw card JSON. Recalled messages retain metadata while their content is cleared. |
| `GET /cards`, `GET /cards/{card_id}` | Return `{"cards":[...]}` or one card object with owning app, raw JSON, element index, streaming mode, last sequence, revision, and expiry timestamps. An inactive stream has a null `streaming_expires_at`. |
| `GET /ws/sessions` | Returns `{"ws_sessions":[...]}` with session, app, device and service IDs, and connection time. |
| `POST /messages/send` | Persists a virtual user's message and returns HTTP 201 with `{message, deliveries}`. Each member bot has a delivery result containing its app ID, event ID, and per-channel status. |
| `POST /cards/action` | Requires `message_id`, `operator_open_id`, and `action.tag`. Accepts SDK action fields `value`, `name`, `option`, `options`, `input_value`, `form_value`, `checked`, and `timezone`, plus optional top-level `host` and `delivery_type`. Returns `{toast, card_applied, card_error, message, delivery}` after callback processing. |
| `POST /events/webhook/verify` | Requires `app_id`. Uses the registered URL or a one-call HTTP(S) `webhook_url` override. Returns `{verified, status_code, error, latency_ms}` for the handshake. A transport failure has status code 0 inside the result. |
| `POST /tokens/expire` | Requires `app_id`. Accepts `token` or a nonempty `tokens` list to expire individual credentials, or expires all credentials for that app when both fields are omitted. Returns `{"expired":n}` without echoing credentials. |

A user message requires one sender selector (`sender_open_id` or
`sender_user_id`), one destination (`chat_id` or `app_id` for a direct
message), and either JSON-encoded string `content` or plain `text`. The default
`msg_type` is `text`. The `mentions` array accepts member open IDs and
`"@bot"`. The `at_bot` flag (alias `mention_bot`) also mentions the member
bots. Text mentions use the Feishu `@_user_N` placeholders and corresponding
event metadata. For replies, `parent_id` must refer to a message in the
destination and `reply_in_thread` requires a parent. Eligible bots receive
`im.message.receive_v1` through the same dispatcher used by Open API events.
Card actions use the dispatcher’s synchronous callback deadline and channel
selection. A failed delivery is represented in the result for the saved
message, callback, or verification. Request validation uses HTTP 400,
missing entities use 404, and conflicts use 409.

### Audit and fault rules

`GET /traces` returns `{"traces":[...]}`, ordered oldest to newest by start
time. Filters are combined and include `direction` (`all`, `inbound`, or
`outbound`), `app_id`, `method`, `path_prefix`, `event_type`, `event_id`,
and `channel` (`webhook` or `websocket`). The `limit` selects the latest
1–256 matching exchanges before ordering. Each trace includes timing,
Feishu code when available, HTTP or acknowledgment status, redacted bodies
and headers, truncation flags, and delivery attempt and duplicate indices
where applicable. Bodies are JSON values or text. `DELETE /traces` clears the
shared ring in both directions.

`POST /faults` creates a rule and returns HTTP 201 with its `fault_id`.
`GET /faults` returns `{"faults":[...]}`. `DELETE /faults/{fault_id}` removes
one rule and returns 404 for an unknown ID. `DELETE /faults` clears all
rules and releases pending fault delays.

Rules contain `kind` and optional top-level `app_id`, `method`,
`path_prefix`, `event_type`, and `channel` (`inbound`, `webhook`, or
`websocket`) matchers. All supplied matchers must match. The first eligible
rule in creation order whose probability trial succeeds wins. The
`probability` default is 1 and valid values are in (0, 1]. Omitted
`remaining_hits` means unlimited. A positive hit count is decremented
atomically and the rule disappears when the last hit is spent. Incoming
requests derive their app from a stored Bearer token or the app selector
of a credential or OAuth endpoint.

| Kind | Scope | Parameters and behavior |
|---|---|---|
| `latency` | Inbound and event delivery | `delay_ms` is 1–60000. Requests continue after a context-cancellable delay. |
| `rate_limit` | Inbound | HTTP 429 with Feishu code `99991400`. `retry_after` is 0–86400 seconds and defaults to 1. |
| `http_error` | Inbound | `status_code` is 400–599 and defaults to 500. Optional `error_code` and `error_msg` default to `99991401` and `internal server error`. |
| `token_expiry` | Inbound Bearer | Evaluated after credential validation. Returns `99991663` for app and tenant tokens or `99991668` for user tokens. The credential remains usable after the fault is spent. |
| `duplicate_event` | Outbound event delivery | `duplicate_count` is 2–10 total copies and defaults to 2. Copies use the same event ID and payload on the matched channel. The first card-action response determines the simulated action result. |

Outbound rules are evaluated per event and transport before its first
attempt. Retries retain the event ID and use the existing retry budget.
URL verification is a separate handshake. Reset cancels pending
deliveries and invalidates the previous audit generation so those
exchanges cannot repopulate the cleared ring.

### Local boundary and data exposure

Use a loopback binding or an isolated test network with trusted clients.
The control plane is unauthenticated and inherits the listener address.
It can change all fixture state and send webhook requests to configured or
overridden HTTP(S) destinations, so the listener belongs inside the local
development or CI trust boundary. The embedded UI can use same-origin
requests against the control plane.

App reads expose identifiers, names, configured URLs, connection counts,
and booleans for presence of webhook encryption and verification
credentials. App secrets, encryption keys, verification tokens, OAuth
credentials, and WebSocket tickets are excluded from control reads.
Audit data uses a header allowlist, credential-field redaction, and bounded
body capture. Application-provided messages and card values remain
visible to the local operator. Keep real credentials out of fixture data,
including URLs and free-form content.

## Consequences

Test code can drive the full SDK lifecycle over HTTP and inspect the same
state the embedded UI will use. Faults are in-memory and resettable. The
control plane gives its caller administrative access to the local test
instance.
