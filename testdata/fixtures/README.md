# Feishu wire fixtures

Each JSON file contains `status` (the HTTP status as a number) and `body` (the
JSON object as sent on the wire). REST and callback response fixtures use the
response status. Outbound event fixtures pair the delivered request body with the
receiver's acknowledgement status. The encrypted URL verification fixture
covers the outer envelope, and the plain fixture also checks the decrypted body.

The fixtures are synthetic sandbox reference examples. Record the source and
capture date in this README when refreshing a fixture from Feishu. Live
responses can differ through optional diagnostics or message context fields.
Preserve those fields in sanitized captures and review their types separately
from the exact sandbox fixture shape.

## Sanitization

Use only a dedicated test tenant, bot, chat, and synthetic messages. Keep raw
captures outside the worktree and out of logs, commits, and pull requests. Copy
only the status and sanitized JSON body. Remove authorization headers, cookies,
real app secrets, access or callback tokens, encryption keys, tenant keys, and
personal information. Use synthetic names, messages, and URLs. Review nested
objects and arrays, URLs and their query strings, and JSON encoded inside strings.

Preserve the wire types, keys, arrays, and nulls. Use these string markers for
values determined at runtime.

| Marker | Replaced value |
| --- | --- |
| `fixture:id` | App, user, chat, message, thread, event, reaction, or other generated identifier |
| `fixture:card_id` | Decimal CardKit entity ID |
| `fixture:millis`, `fixture:micros` | String Unix timestamps in milliseconds or microseconds |
| `fixture:tenant`, `fixture:app_token` | Issued tenant and app access tokens |
| `fixture:nonce` | URL verification challenge |
| `fixture:callback_token` | Generated card-action token. The sandbox sends 32 hex characters. A live callback can carry a `c-` token. |
| `fixture:encrypt` | Encrypted webhook ciphertext |
| `fixture:redacted` | Fixed, synthetic replacement for a provisioned credential or tenant key |

## Capturing and refreshing

1. Configure a self-built app in a dedicated Feishu test tenant. Enable its
   bot, grant permissions for the endpoints under test, and add the bot to a
   dedicated group chat. For event fixtures use a test webhook subscribed to the
   relevant IM events and card callback.
2. Reproduce the request with the Feishu API Explorer or a local HTTP client.
   Record the status and JSON body. For events, record the incoming webhook and
   the receiver's HTTP status. For card actions also record the response body.
   Use separate plaintext and encryption-enabled webhook setups for URL
   verification. Recall created messages and remove test reactions.
3. Replace every sensitive value before copying the JSON into this directory.
   Match the synthetic input used by the corresponding test when refreshing
   fixed values such as message content.
4. Review any structural differences before updating the corresponding sandbox
   behavior. Run the fixture suite and the local and Feishu modes of the tagged
   suite. The hygiene test detects common credential and PII patterns. Review
   the diff for arbitrary secrets, tenant identifiers and personal details
   before staging it.

```sh
go test -race ./tests/sdk_test/... -run TestGolden
FEISHU_LIVE=0 go test -tags=live -count=1 ./tests/live_test/...
FEISHU_LIVE=1 go test -tags=live -count=1 ./tests/live_test/...
```
