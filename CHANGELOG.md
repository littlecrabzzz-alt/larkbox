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
- Sample environment for optional live differential testing.

### Changed

- Clarify the Auth and IM core roadmap and defer evaluation of other Feishu products to later phases.
