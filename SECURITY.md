# Security Policy

## Scope and Intended Use

`larkbox` is designed as a **local development and CI testing sandbox** for Feishu / Lark integrations.

- Do **not** expose `larkbox` (especially the `/_sandbox/*` control plane) directly to the public internet without proper network isolation and authentication.
- Never commit real Feishu production credentials (`app_id`, `app_secret`, `encrypt_key`, `verification_token`) or unsanitized tenant payloads to this repository.

## Reporting a Vulnerability

If you discover a security vulnerability in `larkbox`, please **do not** open a public GitHub issue.

Instead, report it responsibly by emailing `littlecrabzzz@gmail.com` with:

1. A clear description of the vulnerability and its potential impact.
2. Steps to reproduce or a minimal proof of concept.
3. Any suggested remediation if known.

We will acknowledge receipt within 72 hours and work with you to coordinate a fix and disclosure.
