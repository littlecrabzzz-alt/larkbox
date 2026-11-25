# Test with the official CLI

`make test-cli` builds the sandbox and a loopback adapter for the pinned
[lark-cli v1.0.100](https://github.com/larksuite/cli/releases/tag/v1.0.100).
The adapter registers the official transport extension and runs the upstream
commands. It directs Feishu Accounts and Open API requests to the sandbox.
The test runs in Docker without external network access and uses separate
CLI configuration and credential storage for each invocation of the test.

The recipe registers an app, denies and approves separate user login
requests, resolves Bob's open ID by name and email, sends a message from
Alice, and logs out. It asserts the consent list and stored message via
`/_sandbox`.

The same test creates and fetches a document, resolves a Wiki node, uploads
and downloads an identical Drive file, writes and reads a spreadsheet range,
and searches Base records. It obtains IDs and table fields through the CLI
before subsequent operations. In flag mode, Base record search needs an
explicit `--search-field` from `+field-list`. The scopes for that workflow are
listed in [run.py](run.py). The probe checks both inline JSON and the default
NDJSON export to a local file.

## Try the browser flow

On Linux, build both binaries and start the sandbox in one terminal.

```sh
make build
bash tests/lark_cli/build.sh bin/lark-cli-loopback
bin/larkbox --addr 127.0.0.1:8080
```

Use a temporary CLI profile in a second terminal. Open the link printed by
`config init`, choose **Alice**, and select **Approve**.

```sh
larkbox_cli_lab=$(mktemp -d)
export LARKSUITE_CLI_CONFIG_DIR="$larkbox_cli_lab/config"
export LARKSUITE_CLI_DATA_DIR="$larkbox_cli_lab/data"
export LARKBOX_CLI_ORIGIN=http://127.0.0.1:8080
export LARKSUITE_CLI_NO_SKILLS_NOTIFIER=1
export LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1
bin/lark-cli-loopback config init --new --brand feishu --lang en
bin/lark-cli-loopback config set dpop disabled
```

The test profile uses Bearer credentials. Start user login with the contact
search scope and both scopes the pinned CLI's `+messages-send` checks.

```sh
bin/lark-cli-loopback auth login --scope 'contact:user:search im:message im:message.send_as_user' --no-wait --json
```

Open `verification_url` and approve as **Alice**, then pass the `device_code`
from the CLI output to complete login. Scripted clients can instead extract
`user_code` from the URL and send
`{"user_code":"<code>","open_id":"ou_larkbox_alice","decision":"approve"}`
to `POST /_sandbox/auth/devices/decision`.

```sh
bin/lark-cli-loopback auth login --device-code '<device_code>' --json
bin/lark-cli-loopback contact +search-user --as user --query 'bob@example.test' --format json
bin/lark-cli-loopback im +messages-send --as user --user-id '<open_id from search>' --text 'Hello from the sandbox'
bin/lark-cli-loopback auth logout --json
```

Application registration and user login have separate device codes. A code
can be used once, and the sandbox uses `token_ttls.oauth_code` for both
flows. To exercise expiration in a test, configure `clock_start` and advance
the sandbox clock. The interactive prompt shows the target app and scopes
so users can verify their request before approving.
