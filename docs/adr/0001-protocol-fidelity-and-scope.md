# 0001. Auth and IM Core Scope, Go Single Binary, and Protocol Fidelity Strategy

- **Status:** Accepted
- **Date:** 2026-11-25

## Context

Teams integrating bots and AI agents with Feishu / Lark Open Platform need a reliable local sandbox to test authentication, messaging, streaming cards, webhooks, and WebSocket long connections without hitting real tenants.

Previous ad-hoc mock scripts (e.g., thin Python HTTP stubs) suffer from three structural issues:
1. **Protocol drift**: Unvalidated permissive responses hide real Feishu constraints (`content` JSON-string encoding, token expiration, error codes, CardKit `sequence` ordering).
2. **Missing modern transports**: Implementing Feishu's official WebSocket long-connection Protobuf framing is non-trivial in ad-hoc scripts, leaving most modern SDK clients unable to connect.
3. **Scope creep risk**: Attempting to mock the entire Feishu suite (Docs, Bitable, Approval, Calendar, HR) explodes maintenance cost and degrades accuracy.

## Decision

1. **Core Scope**. Prioritize **Authentication (internal tokens, OAuth v1/v2, callback crypto/signing), IM, Bot, Media, CardKit, and dual event delivery (Webhook Schema 2.0 and WebSocket Long Connection)**. Keep the Feishu Open APIs and `/_sandbox/*` control plane modular so downstream test suites can drive the sandbox and future phases can add end-to-end testing features.
2. **Pure Go Single-Binary Architecture**: Implement `larkbox` in pure Go (zero CGO) with an optional embedded Web UI (`//go:embed`) so teams using Python, Node.js, Java, or Go can run it with one binary and zero runtime dependencies. Using Go also aligns directly with Feishu's official `oapi-sdk-go` types and `ws` Protobuf framing.
3. **Strict Mode by Default + Four-Layer Verification**:
   - Enforce real Feishu validation rules and error codes by default.
   - Run official `oapi-sdk-go` (REST + `ws.Client`) integration tests in CI.
   - Maintain sanitized real-Feishu golden fixtures in `testdata/fixtures/`.
   - Maintain an opt-in `//go:build live` differential test harness against a real Feishu Test Tenant to verify protocol parity without requiring live credentials during normal development or CI.

## Deferred / Later Phases

The [planned milestones](../../README.md#planned-milestones) schedule additional
domains after the Auth and IM baseline. Each phase adds verified workflows by
applying the protocol fidelity strategy above.

## Non-Goals

- Pixel-perfect reproduction of the native Feishu client.
- Serving as a production messaging backend.

## Consequences

- **Positive**:
  - High confidence that behavior passing against `larkbox` will behave identically on real Feishu Open Platform.
  - Zero-config drop-in experience for official Feishu SDKs (both Webhook and WebSocket modes).
  - Manageable codebase size with clear acceptance criteria for every new endpoint.
- **Trade-offs**:
  - Strict validation may surface bugs in existing client code that previously passed against permissive mock scripts (considered a feature, not a bug).
