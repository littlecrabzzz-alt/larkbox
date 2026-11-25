# Official SDK integration probes

Each probe starts a fresh larkbox binary on a loopback port. It uses the
language SDK to send a message, connects through the normal WebSocket
bootstrap, and replies through REST from the event handler. The driver checks
that the reply appears in history and the first attempt was acknowledged.

Build larkbox and install the dependencies for the probes you want to run.
The CI workflow runs all three with Node.js 22, Java 17, and Python 3.12.

```sh
make build

npm ci --prefix tests/sdk_external/node --ignore-scripts
python3 tests/sdk_external/run.py node --binary bin/larkbox

mvn -B -q -f tests/sdk_external/java/pom.xml compile dependency:build-classpath -Dmdep.outputFile=target/classpath
python3 tests/sdk_external/run.py java --binary bin/larkbox

python3 -m venv tests/sdk_external/python/.venv
tests/sdk_external/python/.venv/bin/python -m pip install -r tests/sdk_external/python/requirements.txt
python3 tests/sdk_external/run.py python --binary bin/larkbox
```

The driver seeds a predictable `cli_` ID with 16 hexadecimal characters so the
Node.js SDK's long-connection validation accepts it. To use a compatible local
app from another project, set `apps` in a larkbox config file. Seed fields that
refer to the default bot by open ID can keep that ID.

```yaml
apps:
  - app_id: cli_0000000000000001
    app_secret: larkbox-local-secret
    bot_open_id: ou_larkbox_bot
    bot_name: Larkbox Bot
```
