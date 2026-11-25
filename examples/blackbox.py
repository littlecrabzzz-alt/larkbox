#!/usr/bin/env python3
"""Exercise a built Larkbox with real local HTTP, the CLI, and a webhook."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

APP = "cli_larkbox"
SECRET = "larkbox-local-secret"
CHAT = "oc_larkbox_group"
ALICE = "ou_larkbox_alice"
START = "2026-01-01T00:00:00Z"
API = "/open-apis"


def card(text, streaming=False):
    return {
        "schema": "2.0",
        "config": {"streaming_mode": streaming},
        "body": {"elements": [{"tag": "markdown", "element_id": "md", "content": text}]},
    }


class Webhook(ThreadingHTTPServer):
    def __init__(self):
        self.first_event = None
        self.late_received = threading.Event()
        self.release_late = threading.Event()
        self.late_attempted = threading.Event()
        super().__init__(("127.0.0.1", 0), Receiver)

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server_port}/events"


class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        event = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if event.get("type") == "url_verification":
            self.reply(200, {"challenge": event["challenge"]})
            return
        header = event["header"]
        if header["event_type"] == "card.action.trigger":
            self.server.late_received.set()
            self.server.release_late.wait(8)
            try:
                self.reply(200, {"card": {"type": "raw", "data": card("late change")}})
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                self.server.late_attempted.set()
            return
        if self.server.first_event is None:
            self.server.first_event = header["event_id"]
            self.reply(503, {})
        else:
            assert self.server.first_event == header["event_id"]
            self.reply(200, {})

    def reply(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


def wait_for(probe, seconds=8):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = probe()
        if result:
            return result
        time.sleep(0.025)
    raise AssertionError("timed out waiting for a sandbox observation")


class Sandbox:
    def __init__(self, binary, ready, process):
        self.binary = binary
        self.ready = ready
        self.process = process

    @property
    def url(self):
        return "http://" + self.ready.read_text().strip()

    def cli(self, name, *flags, data=None, expected_error=None):
        args = [str(self.binary), "sandbox", name, "--ready-file", str(self.ready), "--timeout", "10s", *flags]
        if data is not None:
            args += ["--data", json.dumps(data)]
        result = subprocess.run(args, capture_output=True, text=True, timeout=12)
        if expected_error:
            assert result.returncode != 0 and expected_error in result.stderr, result.stderr
            return None
        assert result.returncode == 0, result.stderr
        return json.loads(result.stdout)

    def api(self, method, path, data=None, token=None, code=0):
        headers = {"Content-Type": "application/json"}
        if token is not None:
            headers["Authorization"] = "Bearer " + token
        payload = json.dumps(data).encode() if data is not None else None
        req = Request(self.url + API + path, data=payload, headers=headers, method=method)
        try:
            with urlopen(req, timeout=4) as response:
                result = json.load(response)
        except HTTPError as error:
            with error:
                result = json.load(error)
        assert result["code"] == code, f"{path} returned code {result['code']}, expected {code}"
        return result

    def token(self):
        body = {"app_id": APP, "app_secret": SECRET}
        return self.api("POST", "/auth/v3/tenant_access_token/internal", body)["tenant_access_token"]


@contextmanager
def start(binary, directory, name, config):
    ready = directory / f"{name}.ready"
    path = directory / f"{name}.json"
    path.write_text(json.dumps(config))
    env = {name: value for name, value in os.environ.items() if not name.startswith("LARKBOX_")}
    with (directory / f"{name}.log").open("w+") as log:
        proc = subprocess.Popen(
            [str(binary), "--addr", "127.0.0.1:0", "--ready-file", str(ready), "--config", str(path), "--no-ui"],
            env=env, stdout=log, stderr=log,
        )
        try:
            def ready_or_failed():
                if proc.poll() is not None:
                    log.seek(0)
                    raise RuntimeError(log.read())
                return ready.exists()

            wait_for(ready_or_failed)
            yield Sandbox(binary, ready, proc)
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=12)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
                    raise


def exercise(primary, second, receiver, pool):
    assert primary.url != second.url
    assert second.cli("clock")["manual"] is False
    second.cli("advance", "--by", "1s", expected_error="409")
    assert primary.cli("clock") == {"manual": True, "now": START}
    paths = primary.cli("openapi")["paths"]
    for path in ("clock/advance", "traces", "seed", "messages/send", "cards/action"):
        assert f"/_sandbox/{path}" in paths

    primary.cli("seed", data={"apps": [{
        "app_id": APP, "app_secret": SECRET, "bot_open_id": "ou_larkbox_bot",
        "bot_name": "Larkbox Bot", "webhook_url": receiver.url,
    }]})
    assert primary.cli("verify-webhook", data={"app_id": APP})["verified"]

    initial = primary.token()
    primary.api("GET", "/bot/v3/info", token=initial)
    message = {"chat_id": CHAT, "sender_open_id": ALICE, "text": "Retry this event", "at_bot": True}
    send = pool.submit(primary.cli, "send", data=message)
    first = wait_for(lambda: primary.cli("traces", "--event-type", "im.message.receive_v1")["traces"])[0]
    assert first["attempt"] == 1 and first["status_code"] == 503
    event_id = first["event_id"]
    primary.cli("advance", "--by", "9s")
    assert len(primary.cli("traces", "--event-id", event_id)["traces"]) == 1
    primary.api("GET", "/bot/v3/info", token=initial)
    primary.cli("advance", "--by", "1s")
    assert send.result()["deliveries"][0]["event_id"] == event_id
    attempts = primary.cli("traces", "--event-id", event_id)["traces"]
    assert [(x["attempt"], x["status_code"]) for x in attempts] == [(1, 503), (2, 200)]
    assert receiver.first_event == event_id
    primary.api("GET", "/bot/v3/info", token=initial, code=99991663)
    print("PASS: parallel startup, discovery, stable retry ID, and token expiry")

    token = primary.token()
    created = primary.api("POST", "/cardkit/v1/cards", {"type": "card_json", "data": json.dumps(card("start", True))}, token)
    stream = f"/cardkit/v1/cards/{created['data']['card_id']}/elements/md/content"
    primary.api("PUT", stream, {"sequence": 1, "content": "still streaming"}, token)
    primary.cli("advance", "--by", "10m")
    primary.api("PUT", stream, {"sequence": 2, "content": "past stream deadline"}, primary.token(), code=200850)
    primary.cli("advance", "--by", "335h50m")
    primary.api("PUT", stream, {"sequence": 2, "content": "past entity lifetime"}, primary.token(), code=200750)
    print("PASS: CardKit stream deadline and entity lifetime")

    saved = primary.api("POST", "/im/v1/messages?receive_id_type=chat_id", {
        "receive_id": CHAT, "msg_type": "interactive", "content": json.dumps(card("original")),
    }, primary.token())
    message_id = saved["data"]["message_id"]
    click = pool.submit(primary.cli, "card-action", data={
        "message_id": message_id, "operator_open_id": ALICE, "action": {"tag": "button"},
    })
    assert receiver.late_received.wait(5), "callback must reach the receiver"
    try:
        outcome = click.result()
        assert not outcome["card_applied"] and outcome["card_error"] == "delivery timed out"
    finally:
        receiver.release_late.set()
    assert receiver.late_attempted.wait(5), "receiver must attempt its late response"
    traces = primary.cli("traces", "--event-id", outcome["delivery"]["event_id"])["traces"]
    assert len(traces) == 1 and traces[0]["attempt"] == 1
    message = primary.api("GET", "/im/v1/messages/" + message_id, token=primary.token())
    assert "original" in message["data"]["items"][0]["body"]["content"]
    assert "late change" not in message["data"]["items"][0]["body"]["content"]
    primary.cli("reset")
    assert primary.cli("clock")["now"] == START
    print("PASS: actual delivery, late reply ignored, and deterministic reset")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/larkbox"))
    binary = parser.parse_args().binary.resolve()
    with tempfile.TemporaryDirectory(prefix="larkbox-blackbox-") as temp, Webhook() as receiver:
        listener = threading.Thread(target=receiver.serve_forever, daemon=True)
        listener.start()
        try:
            config = {
                "clock_start": START,
                "token_ttls": {"tenant_access_token": 10},
                "event_delivery": {
                    "webhook_timeout_ms": 500,
                    "webhook_max_attempts": 2,
                    "retry_backoff_ms": [10000],
                },
            }
            with start(binary, Path(temp), "first", config) as primary, start(binary, Path(temp), "second", {}) as second:
                with ThreadPoolExecutor(max_workers=1) as pool:
                    exercise(primary, second, receiver, pool)
        finally:
            receiver.release_late.set()
            receiver.shutdown()
            listener.join()


if __name__ == "__main__":
    main()
