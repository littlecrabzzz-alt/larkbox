# larkbox

[![CI](https://github.com/littlecrabzzz-alt/larkbox/actions/workflows/ci.yml/badge.svg)](https://github.com/littlecrabzzz-alt/larkbox/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![Status](https://img.shields.io/badge/Status-Early%20Development-orange)](#roadmap)

**A high-fidelity, protocol-accurate local sandbox and mock server for Feishu / Lark Open Platform — shipped as a single Go binary.**

---

## Table of Contents

- [Goals & Motivation](#goals--motivation)
- [Non-Goals](#non-goals)
- [Core Capabilities](#core-capabilities)
- [Architecture Overview](#architecture-overview)
- [API & Event Compatibility Matrix](#api--event-compatibility-matrix)
- [Verification & Fidelity Strategy](#verification--fidelity-strategy)
- [Roadmap](#roadmap)
- [Documentation & Project Standards](#documentation--project-standards)
- [License](#license)

---

## Goals & Motivation

Teams building Feishu / Lark bots, integrations, and AI agents frequently run into the same testing bottlenecks:

- **Real tenants do not scale for automated testing**: hitting `open.feishu.cn` triggers rate limits, pollutes shared groups, slows feedback loops, and makes edge cases (3-second callback retries, expired tokens, out-of-order streaming updates) hard to reproduce.
- **Ad-hoc mock scripts drift from reality**: hand-rolled mocks usually return `{"code": 0}` unconditionally, omit strict parameter checks, and leave teams unsure whether their integration will actually work against real Feishu.

`larkbox` solves this by providing a **trustworthy, pure-protocol stand-in for Feishu / Lark Open Platform**:

1. **Drop-in compatibility**: Point existing Feishu SDKs (`oapi-sdk-go`, `lark-oapi`, `@larksuiteoapi/node-sdk`) or HTTP clients to `http://127.0.0.1:8080` with zero business code changes.
2. **Protocol accuracy over permissiveness**: Enforce real Feishu validation rules, headers, error codes, encryption/signatures, and wire frames by default (**Strict Mode**).
3. **Verifiable fidelity**: Back every implemented API and event with official SDK integration tests, sanitized golden fixtures, and an opt-in differential live test harness against a real Feishu test tenant.
4. **Zero-friction single binary**: Pure Go (no CGO) with an embedded web chat playground and protocol inspector.

## Non-Goals

To keep fidelity high and maintenance sustainable, `larkbox` explicitly does **not** aim to:

- **Clone the entire Feishu suite**: Docs, Bitable, Approval, Calendar, Contacts/HR, and Mail are out of scope for the core roadmap. `larkbox` focuses deeply on **Auth + IM / Bot / CardKit / Events**.
- **Act as an E2E test runner or assertion framework**: `larkbox` stays completely business-agnostic. Your test suite drives `larkbox` via its `/_sandbox/*` control API or standard Feishu SDKs.
- **Replicate pixel-perfect Feishu card rendering**: The embedded Web UI provides structured card previews, interactive button triggers, streaming updates, and raw JSON/schema inspection—not a 1:1 client layout engine.
- **Serve as a production messaging backend**: `larkbox` is built exclusively for local development, debugging, and CI testing.

## Core Capabilities

- **Authentication & Security**: Internal app tokens (`tenant_access_token`, `app_access_token`), user OAuth flows (`authen/v1`, `authen/v2`), strict Bearer token validation & expiration, and full Webhook AES-256-CBC encryption + `X-Lark-Signature` verification.
- **Stateful IM & Bot Engine**: Virtual apps/bots, users, chats (p2p & group), messages, replies/threads, message editing/patching/recall, emoji reactions, and multipart image/file upload & download.
- **CardKit Streaming Cards**: Card entity creation, streaming element updates with monotonic `sequence` enforcement, settings updates, and interactive `card.action.trigger` callbacks.
- **Dual Event Delivery**:
  - **HTTP Webhook (Schema 2.0)** with `url_verification`, optional encryption/signing, and 3-second timeout retry with identical `event_id`.
  - **WebSocket Long Connections** implementing the official SDK Protobuf framing protocol (`/callback/ws/endpoint` + `/ws/v2`).
- **Control Plane & Fault Injection (`/_sandbox/*`)**: Seed state, simulate user messages / `@bot` mentions / card clicks, inspect request & event traces, and inject latency, rate limits (`99991400`), 500s, token expiry, or duplicate event delivery.

## Architecture Overview

```mermaid
flowchart LR
    Client["App / Agent / Official Feishu SDK"]

    subgraph Server["larkbox (Single Go Binary)"]
        direction TB
        MW["Middleware: Auth / Audit / Fault Injection"]
        API["Feishu Open API\nAuth · OAuth · Bot · IM · Media · CardKit"]
        WS["WS Gateway\n/callback/ws/endpoint + /ws/v2 (Protobuf)"]
        WH["Webhook Dispatcher\nSchema 2.0 · AES · Signature · 3s Retry"]
        Store["Stateful Store\nApps · Users · Chats · Messages · Cards · Media · Traces"]
        Ctrl["Control Plane\n/_sandbox/*"]
        UI["Embedded Web UI\nChat Playground & Protocol Inspector"]
    end

    Client -->|"REST Open API"| MW --> API --> Store
    Client <-->|"WebSocket Protobuf"| WS
    WH -->|"HTTP POST Events"| Client
    Ctrl --> Store
    Ctrl --> WS
    Ctrl --> WH
    UI --> Ctrl
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for detailed design specifications.

## API & Event Compatibility Matrix

Status Legend:
- `Planned` — Scoped for v0.1, not yet implemented
- `Implemented` — Working with strict-mode validation & unit tests
- `SDK-Tested` — Verified end-to-end against official Feishu SDK client
- `Live-Verified` — Verified via differential test against a real Feishu test tenant

### Authentication

| Endpoint | Notes | Status |
|---|---|---|
| `POST /open-apis/auth/v3/tenant_access_token/internal` | App credentials check, TTL, `t-...` token | `Planned` |
| `POST /open-apis/auth/v3/app_access_token/internal` | Issues `a-...` and `tenant_access_token` | `Planned` |
| `GET /open-apis/authen/v1/authorize` & `/v1/index` | OAuth authorize redirect (`code` & `state`) | `Planned` |
| `POST /open-apis/authen/v1/oidc/access_token` | Exchange `code` for `u-...` & `refresh_token` | `Planned` |
| `POST /open-apis/authen/v1/oidc/refresh_access_token` | Refresh user access token | `Planned` |
| `POST /open-apis/authen/v2/oauth/token` | OAuth v2 token exchange & refresh | `Planned` |
| `GET /open-apis/authen/v1/user_info` | Authenticated user profile (`u-...`) | `Planned` |

### Bot, IM, Media & CardKit

| Endpoint | Notes | Status |
|---|---|---|
| `GET /open-apis/bot/v3/info` | Current bot profile & `open_id` | `Planned` |
| `POST / GET /open-apis/im/v1/chats`, `GET /chats/:chat_id` | Create, list, inspect chats | `Planned` |
| `GET / POST / DELETE /open-apis/im/v1/chats/:chat_id/members` | Manage members; emits bot join/leave events | `Planned` |
| `POST /open-apis/im/v1/messages` | Strict `receive_id_type` & JSON-string `content` checks | `Planned` |
| `POST /open-apis/im/v1/messages/:message_id/reply` | Maintains `parent_id`, `root_id`, `thread_id` | `Planned` |
| `PATCH / PUT / DELETE / GET /open-apis/im/v1/messages/:message_id` | Card update, edit, recall, detail | `Planned` |
| `GET /open-apis/im/v1/messages` | History list with sort & cursor pagination | `Planned` |
| `POST / GET / DELETE /open-apis/im/v1/messages/:message_id/reactions` | Emoji reactions | `Planned` |
| `POST / GET /open-apis/im/v1/images` & `/files` | Multipart upload & binary download | `Planned` |
| `GET /open-apis/im/v1/messages/:message_id/resources/:file_key` | Download message attachment resources | `Planned` |
| `POST / PUT / PATCH /open-apis/cardkit/v1/cards*` | Streaming CardKit with monotonic `sequence` check | `Planned` |

### Events & Callbacks

| Event / Transport | Notes | Status |
|---|---|---|
| HTTP Webhook (Schema 2.0) | `url_verification`, AES-256-CBC, `X-Lark-Signature`, 3s retry | `Planned` |
| WebSocket Long Connection | `/callback/ws/endpoint` + `/ws/v2` Protobuf frames | `Planned` |
| `im.message.receive_v1` | p2p, group, `@bot` mentions, replies & threads | `Planned` |
| `im.message.recalled_v1` | Triggered on message recall | `Planned` |
| `im.chat.member.bot.added_v1` / `deleted_v1` | Bot added/removed from chat | `Planned` |
| `card.action.trigger` | Interactive button/form callback with toast/card reply | `Planned` |

## Verification & Fidelity Strategy

To prevent mock drift, `larkbox` uses a four-layer verification pyramid:

1. **Unit & State-Machine Tests**: Table-driven Go tests covering strict validation, error codes, crypto, and concurrent state mutations (`go test -race ./...`).
2. **Official SDK Integration Tests**: End-to-end tests running official `github.com/larksuite/oapi-sdk-go/v3` (both REST and `ws.Client`) directly against an in-process `larkbox` server.
3. **Golden Fixtures**: Sanitized request/response/event captures from real Feishu stored in `testdata/fixtures/`.
4. **Live Differential Tests (`//go:build live`)**: Opt-in suite that executes the exact same test assertions against either local `larkbox` or a real Feishu Test Tenant (`FEISHU_LIVE=1`) to catch upstream protocol changes.

## Roadmap

- **Milestone 1 — Core Engine, Auth & IM APIs**: State store, strict-mode protocol layer, Auth/OAuth v1 & v2, Bearer middleware, Bot/Chat/Message/Reaction/Media APIs, official SDK REST integration tests.
- **Milestone 2 — Dual Event Engine & CardKit**: WebSocket Protobuf long-connection gateway, Webhook Schema 2.0 (encryption, signature, 3s retry), CardKit streaming cards, `card.action.trigger`, official `oapi-sdk-go/ws` end-to-end tests.
- **Milestone 3 — Control Plane, Fault Injection & Live Harness**: `/_sandbox/*` control API, request/event trace logs, chaos rules, and `//go:build live` differential test suite.
- **Milestone 4 — Embedded Web Inspector & Chat Playground**: Lightweight React/Vite UI bundled into the Go binary via `//go:embed`.

## Documentation & Project Standards

- [Architecture & Protocol Design](docs/ARCHITECTURE.md)
- [Architecture Decision Records (ADRs)](docs/adr/README.md)
- [Contributing & Engineering Standards](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)
- [Security Policy](SECURITY.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)

## License

Released under the [MIT License](LICENSE).
