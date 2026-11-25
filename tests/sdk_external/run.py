#!/usr/bin/env python3
"""Verify REST and long-connection delivery with a pinned official SDK."""

import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import time
from urllib.request import Request, urlopen

HERE = Path(__file__).resolve().parent
APP_ID = "cli_0000000000000001"
DEMO_SECRET = "larkbox-local-secret"
CHAT_ID = "oc_larkbox_group"
ALICE = "ou_larkbox_alice"
EVENT_TYPE = "im.message.receive_v1"


@contextmanager
def process(command, log_path):
    with log_path.open("w", encoding="utf-8") as log:
        env = {key: value for key, value in os.environ.items() if not key.startswith("LARKBOX_")}
        proc = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=env)
        try:
            yield proc
        except Exception as error:
            raise AssertionError(f"{command[0]} failed: {error}\n{log_path.read_text()[-4000:]}") from error
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()


def wait_for(proc, observation, description):
    deadline = time.monotonic() + 25
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"process exited {proc.returncode} before {description}")
        result = observation()
        if result:
            return result
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {description}")


def request(origin, path, payload=None):
    body = json.dumps(payload).encode() if payload is not None else None
    req = Request(
        origin + "/_sandbox/" + path, data=body,
        headers={"Content-Type": "application/json"},
    )
    with urlopen(req, timeout=6) as response:
        return json.load(response)


def sdk_command(args):
    if args.sdk == "node":
        return ["node", str(HERE / "node" / "probe.cjs")]
    if args.sdk == "python":
        python = args.python or (HERE / "python" / ".venv" / "bin" / "python")
        return [str(python), str(HERE / "python" / "probe.py")]
    target = HERE / "java" / "target"
    cp = os.pathsep.join([str(target / "classes"), (target / "classpath").read_text().strip()])
    return ["java", "-cp", cp, "Probe"]


def verify(args, directory):
    ready = directory / "ready"
    config = directory / "config.json"
    config.write_text(json.dumps({"apps": [{
        "app_id": APP_ID, "app_secret": DEMO_SECRET, "bot_open_id": "ou_larkbox_bot", "bot_name": "Larkbox Bot",
    }]}))
    command = [
        str(args.binary.resolve()), "--addr", "127.0.0.1:0", "--ready-file", str(ready),
        "--config", str(config), "--no-ui",
    ]
    with process(command, directory / "larkbox.log") as server:
        addr = wait_for(server, lambda: ready.read_text().strip() if ready.exists() else "", "the sandbox ready file")
        origin = "http://" + addr
        marker = args.sdk + " SDK probe"
        command = sdk_command(args) + [origin, APP_ID, DEMO_SECRET, CHAT_ID, marker]
        with process(command, directory / "sdk.log") as client:
            wait_for(client, lambda: request(origin, "ws/sessions")["ws_sessions"], "the SDK long connection")
            outcome = request(origin, "messages/send", {
                "chat_id": CHAT_ID, "sender_open_id": ALICE, "text": marker,
            })
            deliveries = outcome["deliveries"]
            assert len(deliveries) == 1, deliveries
            event_id = deliveries[0]["event_id"]
            assert event_id, deliveries
            assert deliveries[0]["deliveries"] == [{"channel": "websocket", "status_code": 200, "error": ""}], deliveries
            traces = request(origin, "traces?event_id=" + event_id)["traces"]
            assert len(traces) == 1, traces
            assert traces[0]["event_type"] == EVENT_TYPE and traces[0]["attempt"] == 1 and traces[0]["status_code"] == 200, traces
            messages = request(origin, f"chats/{CHAT_ID}/messages")["messages"]
            texts = [json.loads(m["content"])["text"] for m in messages if m["msg_type"] == "text"]
            assert texts == ["ready: " + marker, marker, "reply: " + marker], texts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sdk", choices=["node", "java", "python"])
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--python", help="Override the Python SDK environment")
    args = parser.parse_args()
    with TemporaryDirectory(prefix="larkbox-sdk-") as directory:
        verify(args, Path(directory))
    print(f"PASS {args.sdk} SDK REST calls, websocket delivery, receiver reply and acknowledgement")


if __name__ == "__main__":
    main()
