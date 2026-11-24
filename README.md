# larkbox

A high-fidelity, protocol-accurate local sandbox and mock server for **Feishu / Lark Open Platform** (Auth, IM, Bots, Webhooks, WebSocket Long Connections, and CardKit streaming cards), shipped as a single Go binary.

> **Status:** Early development — repository initialized. Core server implementation is in progress.

## Why larkbox?

Building and testing Feishu/Lark bots and AI agents against real tenants hits friction quickly: rate limits, shared group pollution, complex state setup, slow feedback loops, and flaky ad-hoc mock scripts that diverge from real Feishu behavior.

`larkbox` is a pure protocol stand-in for `open.feishu.cn` / `open.larksuite.com`:

- **Zero code intrusion**: Point your existing Feishu SDK (`oapi-sdk-go`, `lark-oapi`, `@larksuiteoapi/node-sdk`) or HTTP client base URL to `http://127.0.0.1:8080`.
- **Dual event delivery**: Supports both **HTTP Webhook (Schema 2.0)** (with `url_verification`, Verification Token, AES-256-CBC encryption, `X-Lark-Signature`, and 3s timeout retries) and **WebSocket Long Connections** (Protobuf wire protocol compatible with official SDKs).
- **Strict mode by default**: Enforces real Feishu constraints and error codes (`content` JSON-string validation, `receive_id_type` checks, Bearer token TTL/scopes, monotonic CardKit `sequence` checks).
- **Verified against real Feishu**: Tested with official SDK clients and an opt-in differential live test harness (`go test -tags=live ./...`) against real Feishu test tenants.
- **Single binary delivery**: Pure Go with zero CGO and an embedded Web chat & protocol inspector UI.

## Planned Scope (v0.1)

### Authentication & Security
- `POST /open-apis/auth/v3/tenant_access_token/internal`
- `POST /open-apis/auth/v3/app_access_token/internal`
- `GET /open-apis/authen/v1/authorize` (and `/open-apis/authen/v1/index`)
- `POST /open-apis/authen/v1/oidc/access_token` & `refresh_access_token`
- `POST /open-apis/authen/v2/oauth/token`
- `GET /open-apis/authen/v1/user_info`
- Bearer token validation middleware (`t-...`, `a-...`, `u-...`) & Webhook AES-256-CBC encryption / `X-Lark-Signature`

### IM, Bot, Media & CardKit
- `GET /open-apis/bot/v3/info`
- `POST / GET /open-apis/im/v1/chats`, `GET /chats/:chat_id`, `GET / POST / DELETE /chats/:chat_id/members`
- `POST /open-apis/im/v1/messages`, `POST .../reply`, `PATCH`, `PUT`, `DELETE`, `GET` (list & detail)
- `POST / GET / DELETE /open-apis/im/v1/messages/:message_id/reactions`
- `POST / GET /open-apis/im/v1/images`, `POST / GET /open-apis/im/v1/files`, `GET .../messages/:message_id/resources/:file_key`
- `POST /open-apis/cardkit/v1/cards`, `PUT .../elements/:element_id/content`, `PATCH .../settings`, `PUT .../cards/:card_id`

### Events & Callbacks
- `im.message.receive_v1` (p2p, group, `@bot` mentions, replies & threads)
- `im.message.recalled_v1`
- `im.chat.member.bot.added_v1` / `im.chat.member.bot.deleted_v1`
- `card.action.trigger` (with synchronous toast and card update handling)
- WebSocket Long Connection gateway (`/callback/ws/endpoint` + `/ws/v2`)

### Control Plane & Inspector
- `/_sandbox/*` HTTP control API for seeding entities, simulating user messages / `@bot` / card clicks, inspecting request & event traces, and injecting faults (latency, rate limits, token expiry, duplicate event retries).
- Embedded Web UI for interactive chat testing and protocol inspection.

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) and our [Code of Conduct](CODE_OF_CONDUCT.md) before opening a pull request.

## Security

Please review [SECURITY.md](SECURITY.md) for reporting security issues and usage guidelines.

## License

Released under the [MIT License](LICENSE).
