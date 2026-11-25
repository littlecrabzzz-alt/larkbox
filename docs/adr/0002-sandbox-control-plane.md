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
| `GET /openapi.json` | Returns the machine-readable contract for built-in controls. |
| `GET /clock` | Returns the current UTC `now` and whether manual time is configured. |
| `POST /clock/advance` | Requires `{"by":"15s"}` or another positive Go duration. Returns the clock state and wakes due retries. Returns 409 in wall-clock mode. |
| `GET /state` | Returns strict mode, build version, sanitized apps, users, departments, approval definitions, chats, card summaries, active faults, and WebSocket sessions. |
| `POST /seed` | Accepts `apps`, `users`, `departments`, `approval_definitions`, `chats`, and `card_templates` in their configuration shape. Upserts whole entries by primary key and templates by ID and version. Returns counts. Validates references before committing. |
| `POST /reset` | Empty body or `{}` restores the configured startup seed. An optional `{"seed":{...}}` replaces the state for this reset, with omitted lists treated as empty. Both forms clear runtime entities, credentials, traces, faults, and WebSocket tickets and disconnect existing sessions. |
| `GET /chats` | Returns `{"chats":[...]}` with member IDs and settings. |
| `POST /chats/open` | Requires `app_id` and `operator_open_id`. Opens or creates a user-to-bot conversation. Returns `{chat_id, created, event_type, event_id, deliveries, error}`. |
| `GET /chats/{chat_id}/messages` | Returns `{"messages":[...]}` in creation order, including thread replies, sender and mention details, reactions, and available rendered and raw card JSON. Recalled messages retain metadata while their content is cleared. |
| `GET /cards`, `GET /cards/{card_id}` | Return `{"cards":[...]}` or one card object with owning app, raw JSON, element index, streaming mode, last sequence, revision, and expiry timestamps. An inactive stream has a null `streaming_expires_at`. |
| `POST /cards/validate` | Accepts `{"card":{...}}` with inline Card JSON 1.0 or 2.0. Returns `{"valid":true}` or a validation message in the standard error response. |
| `GET /images/{image_key}` | Returns locally uploaded raster image bytes for card previews. |
| `GET /ws/sessions` | Returns `{"ws_sessions":[...]}` with session, app, device and service IDs, and connection time. |
| `POST /messages/send` | Persists a virtual user's message and returns HTTP 201 with `{message, deliveries}`. Each member bot has a delivery result containing its app ID, event ID, and per-channel status. |
| `POST /cards/action` | Requires `message_id`, `operator_open_id`, and `action.tag`. Accepts SDK action fields `value`, `name`, `option`, `options`, `input_value`, `form_value`, `checked`, and `timezone`, plus optional top-level `host` and `delivery_type`. Returns `{toast, card_applied, card_error, message, delivery}` after callback processing. |
| `POST /events/webhook/verify` | Requires `app_id`. Uses the registered URL or a one-call HTTP(S) `webhook_url` override. Uses a one-second control-plane deadline by default. An explicit file or environment verification timeout changes that deadline. Returns `{verified, status_code, error, latency_ms}`. A transport failure has status code 0 inside the result. |
| `POST /events/inject` | Requires `app_id`, `event_type`, and an `event` object. Delivers through the configured receivers regardless of the app's event subscription list. Returns `{event_id, deliveries, error}`. |
| `GET /bots/{app_id}/menu` | Returns the bot's effective menu in Application v7 field names. |
| `POST /bots/{app_id}/menu/click` | Requires `menu_id` and `operator_open_id`. Dispatches the subscribed `application.bot.menu_v6` event for a callback item in an enabled menu. Returns `{event_id, deliveries, error}`. |
| `POST /tokens/expire` | Requires `app_id`. Accepts `token` or a nonempty `tokens` list to expire individual credentials, or expires all credentials for that app when both fields are omitted. Explicit `null` selectors return 400. Returns `{"expired":n}` without echoing credentials. |
| `GET /apps/{app_id}/grants` | Returns `{app_id, items}` with an entry per current user consent. Entries are ordered by `open_id` and include `scope` and `authorized_at`. Returns 404 for an unknown app. |
| `POST /auth/devices/decision` | Requires `user_code` and `decision` (`approve` or `deny`). Approval requires the `open_id` of an active simulated user. Returns `{decision}`, 404 for an unavailable code, or 409 for a second decision. |

A user message requires one sender selector (`sender_open_id` or
`sender_user_id`), one destination (`chat_id` or `app_id` for a direct
message), and either JSON-encoded string `content` or plain `text`. The default
`msg_type` is `text`. The `mentions` array accepts member open IDs and
`"@bot"`. The `at_bot` flag (alias `mention_bot`) also mentions the member
bots. Text mentions use the Feishu `@_user_N` placeholders and corresponding
event metadata. Caller-supplied mention placeholders return 400. For replies,
`parent_id` must refer to a message in the destination and `reply_in_thread`
requires a parent. The message commit captures the eligible member bots.
Those apps receive `im.message.receive_v1` through the same dispatcher used
by Open API events.

An app seed can specify `event_types` as an explicit list. Omission enables the
legacy default events for receive, recall, and bot membership, and an empty array
disables ordinary events. A subscribed chat open dispatches `p2p_chat_create`
for a newly created chat and `im.chat.access_event.bot_p2p_chat_entered_v1` for
an existing one. The first event uses the legacy flat envelope. An existing
conversation returns its latest non-recalled message ID and time in the entry
event when available. For unsubscribed chat opens or menu clicks, the response
shows an empty event ID and delivery list.

The menu mirrors the bot-menu fields in the Application v7 SDK. The sandbox
applies updates immediately using the bot app's own tenant token. A menu item
of type `2` uses a unique `event_key`. Open-link, submenu, and send-message items
are represented in menu configuration. The click control returns HTTP 422 for
these items because the control represents triggering a menu callback.

Card actions use the dispatcher’s synchronous callback deadline and channel
selection. A failed delivery is represented in the result for the saved
message, callback, or verification. Request validation uses HTTP 400,
missing entities use 404, and conflicts use 409.

### User grants and device confirmation

`decision=deny` on the OAuth authorize route sends the documented Feishu
denial callback with `error=access_denied` and the provided `state`. Approval
of browser or device consent records a per-app grant. App registration
creates an app and bot after approval and polling. A user logs in through a
separate device consent request.

`POST /oauth/v1/revoke` uses the Accounts client credentials and form
contract. A successful revocation invalidates all credentials issued for
the app and user consent and removes its grant. The app receives the
sandbox event `larkbox.auth.authorization_revoked_v1` if subscribed. Its
Schema 2.0 event payload includes `open_id` and `scope`. Larkbox returns an
empty HTTP 200 for an unknown, foreign, or previously revoked token without
generating an event. Live verification covers that response for unknown
access and refresh tokens. Grant observation and its event are sandbox
contracts.

`/page/cli` provides local browser confirmation for the CLI. It displays
the app and requested scopes when present and lets an operator choose a
simulated user. Headless tests use the control plane decision endpoint.
The device code goes to the polling client. The confirmation page and
decision endpoint use the separate user code. Reset clears both flows
and all active grants.

### Directory and approval seeds

Department parents, department leaders, and visibility selectors must refer
to entities in the same tenant. `0` is the synthetic root department. The
control plane exposes seeded directory data for operators. Contact v3 reads
enforce the configured caller's visibility.

Approval definitions belong to a tenant and refer to an active approver.
They provide one manual task for each submitted instance. Subscribe to the
definition through Approval v4 and include `approval_instance` in the app's
event types to receive status changes over its webhook. Reset clears the
runtime instances and per-definition subscriptions.

### Timing

Startup settings can replace the default retry intervals, per-transport attempt
counts, and webhook, verification, and acknowledgement deadlines. An optional
RFC3339 start time freezes the sandbox state clock. Advance changes that clock
and signals retries whose delay has elapsed since the failed attempt was
recorded. Advance returns before network delivery finishes. The runtime uses
wall time for network deadlines, webhook signatures, fault delays, and trace latency.
Reset cancels retry waits and restores the configured start time.

### Audit and fault rules

`GET /traces` returns `{"traces":[...]}`, ordered oldest to newest by start
time. Filters are combined and include `direction` (`all`, `inbound`, or
`outbound`), `app_id`, `method`, `path_prefix`, `event_type`, `event_id`,
and `channel` (`webhook` or `websocket`). The `limit` selects the latest
1–256 matching exchanges before ordering. Each trace includes timing,
Feishu code when available, HTTP or acknowledgment status, redacted bodies
and headers, truncation flags, and delivery attempt and duplicate indices
where applicable. Bodies contain canonical, redacted JSON. Opaque, malformed,
and truncated bodies are omitted. `DELETE /traces` clears the
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
of a credential or OAuth endpoint. Inbound selection runs once before
route execution. Expiry requires a valid token accepted by the registered
Bearer policy. Direct `middleware.Bearer` handlers carry that policy.
Use `middleware.WithBearerPolicy(handler, required)` on outer-wrapped
handlers such as `http.TimeoutHandler`, with the same requirement as the
inner Bearer check. Routes without a policy are eligible for non-expiry
faults, including when a request carries a valid Bearer token.

| Kind | Scope | Parameters and behavior |
|---|---|---|
| `latency` | Inbound and event delivery | `delay_ms` is 1–60000. Requests continue after a context-cancellable delay. |
| `rate_limit` | Inbound | HTTP 429 with Feishu code `99991400`. `retry_after` is 0–86400 seconds and defaults to 1. |
| `http_error` | Inbound | `status_code` is 400–599 and defaults to 500. Optional `error_code` and `error_msg` default to `99991401` and `internal server error`. |
| `token_expiry` | Inbound Bearer | Evaluated after credential validation. Returns `99991663` for app and tenant tokens or `99991668` for user tokens. The credential remains usable after the fault is spent. |
| `duplicate_event` | Outbound event delivery | `duplicate_count` is 2–10 total copies and defaults to 2. Copies use the same event ID and payload on the matched channel. The first card-action response determines the simulated action result. |

Outbound rules are evaluated per event and transport before its first
attempt. Retries retain the event ID and use the existing retry budget.
A card action makes one transport attempt by default. An explicit
`duplicate_event` rule adds copies with distinct `duplicate_index` values
and starts each copy at attempt 1. URL verification is a separate
handshake. Reset fences Open API commits by admission generation. A
stale commit returns HTTP 409. Reset also cancels pending deliveries,
drops stale notifications, and prevents old exchanges from repopulating
the audit ring.

### Local boundary and data exposure

Use a loopback binding or an isolated test network with trusted clients.
The control plane is unauthenticated and inherits the listener address.
It can change all fixture state and send webhook requests to configured or
overridden HTTP(S) destinations, so the listener belongs inside the local
development or CI trust boundary. The embedded UI can use same-origin
requests against the control plane.

App reads expose identifiers, names, configured URLs, connection counts,
event subscriptions, and booleans for presence of webhook encryption and verification
credentials. App secrets, encryption keys, verification tokens, OAuth
credentials, and WebSocket tickets are excluded from control reads.
Audit data uses a header allowlist, credential-field redaction, and bounded
body capture. Callback diagnostics also redact known credentials reflected
in display text, localized toast fields, and response object keys. If
scrubbed object keys collide, a numbered suffix keeps each value visible.
Card projections preserve standard
icon identifiers such as `icon.token: "info_outlined"` and still scrub known
credential values. Application-provided messages and card values remain
visible to the local operator. Keep real credentials out of fixture data,
including URLs and free-form content.

## Consequences

Test code can drive the full SDK lifecycle over HTTP and inspect the same
state the embedded UI will use. Faults are in-memory and resettable. The
control plane gives its caller administrative access to the local test
instance.
