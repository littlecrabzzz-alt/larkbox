# Contributing & Engineering Standards

Thank you for contributing to `larkbox`! This document defines our project management workflow, coding conventions, and quality bars so every contribution keeps the sandbox trustworthy and maintainable.

---

## 1. Guiding Principles

1. **Protocol fidelity over convenience.** When simulating Feishu / Lark APIs, match real validation rules, headers, field types, and error codes rather than returning permissive stubs.
2. **Keep business logic out.** `larkbox` is a pure Feishu protocol sandbox. Domain-specific test logic belongs in caller repositories, not in `larkbox`.
3. **Small, focused, reversible changes.** Prefer vertical slices (one endpoint group or one protocol capability with tests) over monolithic PRs.
4. **English by default.** All code, comments, commit messages, PR descriptions, issues, and repository documentation must be written in English.

---

## 2. Branching & Git Workflow (Trunk-Based Development)

- `main` is always releasable and protected by CI (`gofmt`, `go vet`, `go test -race`, `go build`).
- Create short-lived branches from `main` using descriptive prefixes:
  - `feat/<short-topic>` (e.g., `feat/im-message-reply`)
  - `fix/<short-topic>` (e.g., `fix/ws-ping-pong-header`)
  - `docs/<short-topic>` (e.g., `docs/architecture-spec`)
  - `refactor/<short-topic>` / `test/<short-topic>` / `chore/<short-topic>`
- Rebase or merge `main` cleanly before merging. Prefer **Squash and Merge** so `main` retains a linear, readable commit history.

---

## 3. Commit & PR Title Convention

We follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/). Both commit messages and PR titles must follow:

```text
<type>(<optional-scope>): <imperative lowercase summary>
```

### Allowed Types
- `feat`: New Feishu endpoint, event type, or sandbox capability
- `fix`: Bug fix or protocol discrepancy fix
- `docs`: Documentation, README, ADRs, comments
- `test`: Adding or updating unit, SDK integration, or live differential tests
- `refactor`: Code restructuring without behavior changes
- `perf`: Performance improvement
- `chore`: Build scripts, CI, dependencies, repository tooling

### Recommended Scopes
`auth`, `oauth`, `im`, `bot`, `media`, `cardkit`, `webhook`, `ws`, `store`, `sandbox`, `ui`, `ci`, `deps`

### Examples
- `feat(auth): implement tenant_access_token internal endpoint and TTL validation`
- `fix(im): reject non-JSON-string content with error code 99992402`
- `test(ws): add oapi-sdk-go long connection end-to-end test`
- `docs(adr): add ADR-0001 on protocol fidelity and bounded scope`

---

## 4. Go Coding & Testing Standards

- **Zero CGO**: Keep the server 100% pure Go so cross-compilation (`darwin`/`linux`/`windows`, `amd64`/`arm64`) stays frictionless.
- **Formatting & Linting**: Run `gofmt`, `go vet ./...`, and `golangci-lint run` (configured via `.golangci.yml`).
- **Concurrency Safety**: All shared store mutations and event registries must be thread-safe and pass `go test -race ./...`.
- **Error Handling**:
  - Wrap internal errors with context (`fmt.Errorf("...: %w", err)`).
  - Return structured Feishu API errors (`code`, `msg`, HTTP status) from `internal/protocol`; never leak raw Go stack errors into API responses.
- **Strict Mode Validation**: Every new handler must validate required headers, token type/expiry, query params, and payload constraints just like real Feishu.
- **Testing Pyramid**:
  - Unit tests (`*_test.go`): Table-driven tests covering happy paths and error-code edge cases.
  - SDK integration tests (`tests/sdk_test/`): Verify compatibility with official `oapi-sdk-go`.
  - Live differential tests (`tests/live_test/`, guarded by `//go:build live`): Compare `larkbox` vs real Feishu test tenant behavior when introducing or changing protocol primitives.

---

## 5. Architecture Decision Records (ADRs)

Any non-trivial architectural change (storage engine, wire protocol strategy, public `/_sandbox/*` API contract, dependency additions with significant footprint) must be documented in a short ADR under `docs/adr/` before or alongside implementation.

---

## 6. Definition of Done (DoD)

A PR is ready to merge when:

- [ ] Code compiles cleanly and passes `gofmt`, `go vet ./...`, and `go test -race ./...`
- [ ] Strict validation and error codes match real Feishu behavior
- [ ] Official SDK test or unit tests cover both valid and invalid inputs
- [ ] `README.md` Compatibility Matrix and `CHANGELOG.md` (`[Unreleased]`) are updated for user-facing changes
- [ ] No real credentials, secrets, or unsanitized tenant payloads are included

---

## 7. Versioning & Releases

- We follow [Semantic Versioning 2.0.0](https://semver.org/):
  - `0.x.y` during initial development; breaking changes to `/_sandbox/*` or CLI flags bump minor (`0.X.0`) and are explicitly noted in `CHANGELOG.md`.
  - `1.0.0` marks a stable core protocol surface and control plane API.
- Releases are cut via annotated Git tags (`v0.1.0`) from a green `main` branch, accompanied by updated `CHANGELOG.md` entries and prebuilt cross-platform binaries on GitHub Releases.
