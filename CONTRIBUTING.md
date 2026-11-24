# Contributing to larkbox

Thank you for your interest in contributing to `larkbox`!

## Guiding Principles

`larkbox` exists to be a **protocol-accurate** stand-in for Feishu / Lark Open Platform. When adding endpoints, events, or validation rules:

1. **Match real Feishu behavior over convenience.** Prefer strict parameter checks, real error codes, and authentic payload shapes over permissive stubs.
2. **Keep business logic out.** `larkbox` only simulates Feishu platform primitives and sandbox control APIs.
3. **Verify with official SDKs.** New endpoints and events should be covered by tests using official Feishu SDK clients (such as `oapi-sdk-go`).
4. **Back changes with golden fixtures.** Whenever possible, validate edge cases against a real Feishu test tenant and add sanitized fixtures under `testdata/fixtures/`.

## Development Setup

### Prerequisites
- Go 1.24+ (pure Go, no CGO required)
- Node.js 20+ (only needed when working on the embedded Web UI under `web/`)

### Running Tests

```bash
# Run unit and official SDK integration tests (offline)
go test -race ./...

# Run differential tests against a real Feishu test tenant (opt-in)
FEISHU_APP_ID=cli_xxx FEISHU_APP_SECRET=xxx go test -tags=live ./tests/live_test/...
```

## Commit & Pull Request Conventions

- **Language:** Write all commit messages, PR titles, code comments, and documentation in **English**.
- **Commit Format:** Follow [Conventional Commits](https://www.conventionalcommits.org/):
  - `feat(im): support message reply thread fields`
  - `fix(ws): handle control frame pong header encoding`
  - `test(auth): add live differential test for expired tenant token`
  - `docs: update API compatibility matrix`
- **Pull Requests:**
  1. Fork the repository and create a feature branch from `main`.
  2. Keep PRs focused on a single feature, endpoint group, or bug fix.
  3. Ensure `go test -race ./...` and `go vet ./...` pass cleanly.
  4. Fill out the PR template checklist, noting any real Feishu verification performed.

## Reporting Protocol Discrepancies

If `larkbox` behaves differently from real Feishu Open Platform (status code, error code, JSON field name/type, header, or WebSocket frame), please open a **Bug Report** with both the real Feishu payload (sanitized) and the `larkbox` payload.
