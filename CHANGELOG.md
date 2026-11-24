# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial repository setup, open-source templates, CI workflow, and governance documentation.
- Project architecture specification and initial Architecture Decision Record (ADR-0001).
- Pure-Go CLI, YAML/JSON configuration with local seed data, and a health endpoint with graceful shutdown.
- Feishu response, ID, and webhook crypto helpers, plus an in-memory store with token expiry, CardKit sequencing, and audit traces.
- Bearer authentication, inbound auditing, and fault-injection middleware hooks.
- Auth v3 internal app and tenant tokens, user OAuth v1/v2 with registered callbacks, PKCE, single-use grants, v2 offline access, and authenticated user profiles.
- Official Feishu Go SDK integration tests for the complete authentication flow.
- Bot profiles, chats and membership, message history with replies and threads, edits, recalls, and emoji reactions.
- Image and file uploads and binary downloads, including resources referenced by messages.
- In-process IM event hooks for bot membership changes and message recalls.
- Official Feishu Go SDK REST tests for Bot, IM, reactions, and media.
- SDK-compatible WebSocket event delivery with single-use tickets, Protobuf ping/pong, and correlated client acknowledgements.
- Schema 2.0 webhooks with URL verification, optional encryption and signing, and retries that preserve the event ID.
- Outbound audit traces for WebSocket and webhook attempts, with official Go SDK coverage for received and recalled messages and bot membership events.
- CardKit v1 card creation, text streaming, settings, element edits, and atomic batch updates with app ownership, monotonic sequences, and scoped UUID replay.
- Card entity references in interactive messages and replies, with the current card reflected in message reads.
- Synchronous `card.action.trigger` callbacks over WebSocket or signed, encrypted webhooks, with toast capture, validated card updates, and outbound audit traces.
- Official Feishu Go SDK integration tests for CardKit REST endpoints and both callback transports.
- Registered local CardKit templates, entity and stream expiration, and IM card content read modes.
- Local `/_sandbox/*` control plane for runtime seeds, reset, message and card simulation, webhook verification, and state inspection.
- Filterable and clearable audit traces, configurable inbound and outbound faults, and direct credential expiry.
- Black-box official Go SDK coverage for sandbox mentions, card replies and callbacks, fault recovery, and reset.
- Sample environment for optional live differential testing.

### Fixed

- Fence stale Open API writes and event notifications after sandbox reset.
- Redact structured audit and card diagnostics consistently, apply one inbound fault per request, and validate sandbox seed and token selectors before changing state.
- Restrict live card entity reads to the owning app and reject stale callback overwrites and conflicting UUID reuse.
- Redact outbound callback credentials in audit bodies while retaining authenticated wire delivery.

### Changed

- Clarify the Auth and IM core roadmap and defer evaluation of other Feishu products to later phases.
