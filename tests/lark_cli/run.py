#!/usr/bin/env python3
"""Run device registration, login, IM and logout with the official CLI."""

import argparse
from contextlib import contextmanager
import json
from pathlib import Path
import re
import subprocess
from tempfile import TemporaryDirectory
import time
from urllib.parse import parse_qs, urlencode, urlsplit
from urllib.request import ProxyHandler, Request, build_opener


ALICE = "ou_larkbox_alice"
BOB = "ou_larkbox_bob"
SCOPE = "contact:user:search im:message im:message.send_as_user"
DEMO_APP = "cli_larkbox"
HTTP = build_opener(ProxyHandler({}))


@contextmanager
def running(command, directory, name, env):
    stdout = directory / f"{name}.stdout"
    stderr = directory / f"{name}.stderr"
    with stdout.open("w") as out, stderr.open("w") as err:
        proc = subprocess.Popen(command, cwd=directory, env=env,
                                stdin=subprocess.DEVNULL, stdout=out, stderr=err)
        try:
            yield proc, stdout, stderr
        except Exception as error:
            raise AssertionError(f"{name}: {error}\n{stdout.read_text()[-2000:]}\n{stderr.read_text()[-2000:]}") from error
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()


def wait_for(proc, observe, description):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        result = observe()
        if result:
            return result
        if proc.poll() is not None:
            raise AssertionError(f"exited {proc.returncode} before {description}")
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {description}")


def get_json(origin, path):
    with HTTP.open(origin + "/_sandbox/" + path, timeout=5) as resp:
        return json.load(resp)


def decide(origin, url, title, decision):
    target = urlsplit(url)
    assert f"{target.scheme}://{target.netloc}" == origin and target.path == "/page/cli", url
    code, = parse_qs(target.query)["user_code"]
    with HTTP.open(url, timeout=5) as resp:
        page = resp.read().decode()
        assert title in page and code in page and ALICE in page, page
    body = urlencode({"user_code": code, "open_id": ALICE, "decision": decision}).encode()
    with HTTP.open(Request(origin + "/page/cli", data=body), timeout=5) as resp:
        assert ("Request denied" if decision == "deny" else "Request approved") in resp.read().decode()


def verify(args, directory):
    ready = directory / "ready"
    server_cmd = [str(args.binary.resolve()), "--addr", "127.0.0.1:0", "--ready-file", str(ready)]
    with running(server_cmd, directory, "sandbox", {}) as (server, _, _):
        origin = "http://" + wait_for(server, lambda: ready.read_text().strip() if ready.exists() else "", "the ready file")
        env = {
            "LANG": "C.UTF-8",
            "LARKBOX_CLI_ORIGIN": origin,
            "LARKSUITE_CLI_CONFIG_DIR": str(directory / "cli-config"),
            "LARKSUITE_CLI_DATA_DIR": str(directory / "cli-data"),
            "LARKSUITE_CLI_NO_SKILLS_NOTIFIER": "1",
            "LARKSUITE_CLI_NO_UPDATE_NOTIFIER": "1",
        }
        cli = str(args.cli.resolve())

        def command(*argv, success=True):
            result = subprocess.run([cli, *argv], cwd=directory, env=env, stdin=subprocess.DEVNULL,
                                    capture_output=True, text=True, timeout=25, check=False)
            assert (result.returncode == 0) == success, f"{argv[:2]}: {result.stdout}\n{result.stderr}"
            return result.stdout

        with running([cli, "config", "init", "--new", "--brand", "feishu", "--lang", "en"],
                     directory, "registration", env) as (registration, out, err):
            pattern = re.compile(re.escape(origin) + r"/page/cli\?user_code=[-A-Z0-9]+[^\s]*")
            link = wait_for(registration, lambda: pattern.search(err.read_text()), "the CLI verification link")
            decide(origin, link[0], "Register a sandbox app", "approve")
            assert registration.wait(timeout=25) == 0, err.read_text()
            app = json.loads(out.read_text())["appId"]
        assert app != DEMO_APP
        assert any(entry["app_id"] == app and entry["bot_open_id"].startswith("ou_")
                   for entry in get_json(origin, "state")["apps"])
        grants_path = f"apps/{app}/grants"
        assert get_json(origin, grants_path)["items"] == []

        command("config", "set", "dpop", "disabled")
        for decision in ("deny", "approve"):
            issued = json.loads(command("auth", "login", "--scope", SCOPE, "--no-wait", "--json"))
            decide(origin, issued["verification_url"], "Authorize a sandbox app", decision)
            command("auth", "login", "--device-code", issued["device_code"], "--json", success=decision == "approve")
            grants = get_json(origin, grants_path)["items"]
            expected = [(ALICE, SCOPE + " offline_access")] if decision == "approve" else []
            assert [(entry["open_id"], entry["scope"]) for entry in grants] == expected, grants
        assert get_json(origin, f"apps/{DEMO_APP}/grants")["items"] == []

        recipient = ""
        for query in ("Bob", "bob@example.test"):
            found = json.loads(command("contact", "+search-user", "--as", "user", "--query", query, "--format", "json"))
            matches = found["data"]["users"]
            assert len(matches) == 1 and matches[0]["open_id"] == BOB, matches
            recipient = matches[0]["open_id"]
        marker = "Official CLI sandbox message"
        sent = json.loads(command("im", "+messages-send", "--as", "user", "--user-id", recipient, "--text", marker))
        messages = get_json(origin, "chats/" + sent["data"]["chat_id"] + "/messages")["messages"]
        assert len(messages) == 1 and messages[0]["message_id"] == sent["data"]["message_id"], messages
        assert messages[0]["sender"]["id"] == ALICE and json.loads(messages[0]["content"])["text"] == marker, messages

        assert json.loads(command("auth", "logout", "--json"))["loggedOut"]
        assert get_json(origin, grants_path)["items"] == []
        version = command("--version").strip()
    print(f"PASS {version} app registration, denial, login, recipient search, user message and logout")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--cli", required=True, type=Path)
    args = parser.parse_args()
    with TemporaryDirectory(prefix="larkbox-cli-") as directory:
        verify(args, Path(directory))


if __name__ == "__main__":
    main()
