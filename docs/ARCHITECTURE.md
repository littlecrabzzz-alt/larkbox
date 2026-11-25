# Architecture & Protocol Design

This document describes the internal architecture of `larkbox`, its protocol fidelity guarantees, and how modules interact.

---

## 1. Module Boundaries

```text
cmd/larkbox/          CLI entrypoint (flags, config loading, seed bootstrap, HTTP server lifecycle)
internal/
  config/             Configuration schema, defaults, and pre-seeded virtual entities
  protocol/           Feishu response envelopes, error codes, ID generators, AES-256-CBC & signature crypto
  wsproto/            Protobuf Frame codec matching official oapi-sdk-go/ws (pbbp2)
  store/              Thread-safe in-memory state store (Apps, Tokens, Users, Chats, Messages, Cards, Media, Traces)
  feishu/
    middleware/       Bearer token validation, request/response audit logging, fault injection
    auth/             internal tokens, authen v1/v2 OAuth and Accounts endpoints
    bot/              bot/v3/info
    im/               chats, members, messages, reply/thread, reactions, images & files
    cardkit/          cardkit/v1 streaming card APIs
  event/              Unified event router (HTTP Webhook Schema 2.0 + WebSocket long-connection gateway)
  sandbox/            /_sandbox/* control plane HTTP APIs
  server/             Router wiring, graceful shutdown, and embedded static UI serving
web/                  Lightweight Vite + React chat playground & protocol inspector (embedded via //go:embed)
tests/
  sdk_test/           Offline end-to-end integration tests using official oapi-sdk-go (REST + WS)
  lark_cli/           Pinned official CLI onboarding and IM with loopback transport
  live_test/          //go:build live differential tests for the sandbox and a Feishu test tenant
testdata/fixtures/    Sanitized REST and webhook wire fixtures
```

---

## 2. Core State Model (`internal/store`)

All entities are scoped cleanly to support concurrent testing across multiple chats or apps without state leakage:

- **App / Bot**: `app_id`, `app_secret`, `bot_open_id`, `verification_token`, `encrypt_key`, webhook target URL, active WebSocket connections.
- **Tokens**: `tenant_access_token` (`t-...`), `app_access_token` (`a-...`), `user_access_token` (`u-...`), `refresh_token`, OAuth authorization `code`, each with explicit `app_id`, `expires_at`, and revocation flag.
- **Authorizations**. Each active grant links an app and a user. Pending exchanges and issued credentials carry the same grant ID. Revocation invalidates that grant's credentials and outstanding exchanges before emitting its event.
- **Device flows**. App registration and user consent have distinct code pairs, poll deadlines, and one-time completion. Device codes use the sandbox state clock and the configured OAuth code TTL.
- **Users**: `open_id` (`ou_...`), `user_id`, `union_id` (`on_...`), name, email, avatar.
- **Chats**: `chat_id` (`oc_...`), `chat_mode` (`p2p` / `group`), name, owner, member set.
- **Messages**: `message_id` (`om_...`), `chat_id`, `root_id`, `parent_id`, `thread_id`, `msg_type`, raw JSON-string `content`, `mentions`, `deleted` (recalled), `updated`, timestamps, reactions list.
- **Cards (CardKit)**: `card_id`, owning app, card JSON or template data, indexed element IDs, `streaming_mode`, and last accepted `sequence` (strictly monotonic per card). The sending app can resolve the linked entity on message reads. Other apps receive the stored message representation.
- **Media**: Binary blob store keyed by `image_key` (`img_v3_...`) and `file_key` (`file_v3_...`) with `Content-Type` and filename metadata.
- **Audit Traces**: Ring buffer / queryable log of inbound Open API calls (method, path, headers, body, status, Feishu `code`, latency) and outbound event deliveries (channel, payload, response status/body, retry attempts).

---

## 3. Strict Mode Contract Rules

By default, `larkbox` enforces real Feishu API constraints:

1. **Authentication**: Missing/invalid/expired tokens return standard Feishu error codes (`99991661`, `99991663`, `99991668`, etc.) and reject mismatched token types (e.g., calling `/authen/v1/user_info` with a `tenant_access_token`).
2. **Message Content Encoding**: `POST /open-apis/im/v1/messages` requires `content` to be a JSON-encoded string (not a raw JSON object), returning `99992402` on schema violations.
3. **ID Type Validation**: `receive_id_type` (`chat_id`, `open_id`, `user_id`, `union_id`, `email`) is validated against target existence and prefix conventions.
4. **CardKit Monotonicity**: Every mutation requires a positive `sequence` greater than the last committed one. An identical request replays a successful UUID within one hour, scoped to the app, card, and operation. Reuse with a different canonical request returns a UUID conflict. Batch actions commit together with the sequence under the store lock.
5. **Webhook Crypto & Retries**:
   - Encryption key: `SHA-256(encrypt_key)`, AES-256-CBC with random 16-byte IV prepended + PKCS7 padding, Base64 encoded into `{"encrypt":"..."}`.
   - Signature header: `X-Lark-Signature = hex(SHA-256(timestamp + nonce + encrypt_key + raw_body))`.
   - Delivery timeout defaults to 3 seconds; non-2xx or timeout retries with the exact same `event_id`.

---

## 4. Dual Event Delivery (`internal/event` + `internal/wsproto`)

When an event occurs (e.g., user sends a message via `/_sandbox/messages/send`, bot joins a chat, or user clicks a card button):

1. Event payload is constructed in authentic **Feishu Schema 2.0** format (including `@_user_1` mention placeholders and `mentions` metadata for `@bot` messages).
2. The `Dispatcher` inspects the target `app_id`:
   - **WebSocket channel**: If one or more official SDK `ws.Client` sessions are connected to `/ws/v2` for that app, the event is serialized into a `pbbp2.Frame` (`method=1`, header `type=event`) and delivered to one active session for that app. Failed acknowledgements and timeouts can be retried with the same event ID.
   - **Webhook channel**: If a webhook URL is configured for the app, the event is dispatched via HTTP POST with optional encryption, signature headers, and retry tracking.
3. Card actions use one delivery attempt with a three-second deadline. An active WebSocket session takes precedence over the webhook. The pinned Go SDK dispatches `card.action.trigger` from a `type=event` frame and puts the callback JSON in the base64 `data` field of its ACK. Larkbox returns the toast and applies valid card updates only when both the message and linked card revisions still match the click snapshot. Replacement of a linked card requires its streaming window to be closed.
4. Every delivery attempt, latency, and client response is recorded in the Audit Trace store for inspection. Outbound audit bodies redact protocol credentials before capture limits are applied.

---

## 5. Four-Layer Verification Pyramid

| Layer | Command | Purpose |
|---|---|---|
| 1. Unit & State Tests | `go test -race ./internal/...` | Validate strict rules, crypto, error codes, and race safety |
| 2. Official SDK Tests | `go test -race ./tests/sdk_test/...` | Prove wire compatibility with `oapi-sdk-go` REST & WS clients |
| 3. Golden Fixtures | `testdata/fixtures/` | Check payload shapes, types, and HTTP and business codes |
| 4. Live Differential Tests | `go test -tags=live ./tests/live_test/...` | Compare `larkbox` vs real Feishu Test Tenant behavior on demand |
