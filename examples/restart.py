#!/usr/bin/env python3
"""Verify durable state with an actual stop, restart, and snapshot import."""

import argparse
import json
from pathlib import Path
import tempfile
import threading
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from blackbox import APP, CHAT, SECRET, START, Webhook, card, start, wait_for


OPERATOR = "local-restart-probe-token"
MARKER = "durable test fixture"


def snapshot(server, method="GET", data=None, credential=OPERATOR, want=200):
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + credential}
    req = Request(server.url + "/_sandbox/snapshot", method=method, data=data, headers=headers)
    try:
        response = urlopen(req, timeout=6)
    except HTTPError as error:
        response = error
    with response:
        assert response.status == want, response.status
        return response.read()


def advance(server, seconds):
    request = Request(server.url + "/_sandbox/clock/advance", method="POST",
                      headers={"Content-Type": "application/json", "Authorization": "Bearer " + OPERATOR},
                      data=json.dumps({"by": f"{seconds}s"}).encode())
    with urlopen(request, timeout=4) as response:
        assert json.load(response)["manual"]


def exercise(binary, directory, receiver):
    state = directory / "sandbox-state.json"
    config = {
        "state_file": str(state), "sandbox_token": OPERATOR, "clock_start": START,
        "token_ttls": {"tenant_access_token": 20},
        "event_delivery": {"webhook_max_attempts": 2, "retry_backoff_ms": [10000]},
        "apps": [{
            "app_id": APP, "app_secret": SECRET, "bot_open_id": "ou_larkbox_bot",
            "bot_name": "Larkbox Bot", "webhook_url": receiver.url,
            "event_types": ["im.message.recalled_v1"],
        }],
    }
    with start(binary, directory, "before-crash", config) as server:
        token = server.token()

        def send(text):
            return server.api("POST", "/im/v1/messages?receive_id_type=chat_id", {
                "receive_id": CHAT, "msg_type": "text", "content": json.dumps({"text": text}),
            }, token)["data"]["message_id"]

        kept = send(MARKER)
        recalled = send("retry fixture")
        created = server.api("POST", "/cardkit/v1/cards",
                             {"type": "card_json", "data": json.dumps(card(MARKER, True))}, token)
        stream = f"/cardkit/v1/cards/{created['data']['card_id']}/elements/md/content"
        server.api("PUT", stream, {"sequence": 1, "content": "before restart"}, token)
        server.api("DELETE", "/im/v1/messages/" + recalled, token=token)
        event_id = wait_for(lambda: receiver.first_event)
        wait_for(lambda: any(item["EventID"] == event_id and item["Attempt"] == 2
                             for item in json.loads(state.read_bytes())["pending_retries"]))
        assert state.stat().st_mode & 0o077 == 0
        server.process.kill()
        server.process.wait(timeout=6)

    with start(binary, directory, "after-crash", config) as server:
        message = server.api("GET", "/im/v1/messages/" + kept, token=token)
        assert MARKER in message["data"]["items"][0]["body"]["content"]
        server.api("PUT", stream, {"sequence": 2, "content": "after restart"}, token)
        saved = json.loads(snapshot(server))
        assert saved["version"] == 1 and saved["manual_clock"] == START
        assert [(entry["EventID"], entry["Attempt"]) for entry in saved["pending_retries"]] == [(event_id, 2)]
        advance(server, 10)

        def completed_snapshot():
            current = json.loads(snapshot(server))
            return current if not current["pending_retries"] else None

        restored = wait_for(completed_snapshot)
        assert restored["manual_clock"] == "2026-01-01T00:00:10Z"
        completed = snapshot(server)
        snapshot(server, credential=token, want=401)
        snapshot(server, "POST", completed.replace(b'"version":1', b'"version":99', 1), want=400)
        advance(server, 10)
        server.api("GET", "/bot/v3/info", token=token, code=99991663)
        snapshot(server, "POST", completed)
        server.api("GET", "/bot/v3/info", token=token)

    with start(binary, directory, "after-import", config) as server:
        server.api("GET", "/bot/v3/info", token=token)
        assert json.loads(snapshot(server))["pending_retries"] == []
        advance(server, 10)
        server.api("GET", "/bot/v3/info", token=token, code=99991663)

    print("PASS: process crash recovery, pending callback retry, CardKit sequence, token expiry, and snapshot import")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/larkbox"))
    binary = parser.parse_args().binary.resolve()
    with tempfile.TemporaryDirectory(prefix="larkbox-restart-") as temp, Webhook() as receiver:
        listener = threading.Thread(target=receiver.serve_forever, daemon=True)
        listener.start()
        try:
            exercise(binary, Path(temp), receiver)
        finally:
            receiver.shutdown()
            listener.join(timeout=3)


if __name__ == "__main__":
    main()
