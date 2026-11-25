#!/usr/bin/env python3
"""Run local workflows end to end with the official CLI."""

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
SCOPES = (
    "base:app:create", "base:field:read", "base:record:create", "base:record:read",
    "base:table:create", "base:table:delete", "base:table:read", "base:table:update",
    "calendar:calendar.event:create", "calendar:calendar.event:delete",
    "calendar:calendar.event:read", "calendar:calendar.event:update",
    "contact:user:search", "docs:permission.member:auth", "docx:document:create", "docx:document:readonly",
    "drive:drive.metadata:readonly", "drive:file:download", "drive:file:upload",
    "im:message", "im:message.send_as_user", "sheets:spreadsheet:create",
    "mail:user_mailbox:readonly", "mail:user_mailbox.message:send", "mail:user_mailbox.message:modify",
    "mail:user_mailbox.message:readonly", "mail:user_mailbox.message.address:read",
    "mail:user_mailbox.message.subject:read", "mail:user_mailbox.message.body:read",
    "minutes:minutes.basic:read", "minutes:minutes.artifacts:read", "task:task:write",
    "sheets:spreadsheet:read", "sheets:spreadsheet:write_only", "wiki:node:create",
    "wiki:node:read", "wiki:node:retrieve", "wiki:space:read", "wiki:space:write_only",
)
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


def seed(origin, **data):
    request = Request(origin + "/_sandbox/seed", data=json.dumps(data).encode(),
                      headers={"Content-Type": "application/json"})
    with HTTP.open(request, timeout=5) as resp:
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
            issued = json.loads(command("auth", "login", "--scope", " ".join(SCOPES), "--no-wait", "--json"))
            decide(origin, issued["verification_url"], "Authorize a sandbox app", decision)
            command("auth", "login", "--device-code", issued["device_code"], "--json", success=decision == "approve")
            grants = get_json(origin, grants_path)["items"]
            expected = [(ALICE, " ".join(sorted((*SCOPES, "offline_access"))))] if decision == "approve" else []
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

        verify_content(command, directory)
        verify_work(command, directory, origin)
        assert json.loads(command("auth", "logout", "--json"))["loggedOut"]
        assert get_json(origin, grants_path)["items"] == []
        version = command("--version").strip()
    print(f"PASS {version} registration, login, directory, IM, Docs, Wiki, Drive, Sheets, Base, Calendar, Tasks, Minutes, Mail and logout")


def verify_content(command, directory):
    def call(*argv):
        result = json.loads(command(*argv, "--as", "user"))
        assert result["ok"], result
        return result["data"]

    marker = "CLI sandbox content"
    doc = call("docs", "+create", "--api-version", "v2", "--title", "Probe document",
               "--doc-format", "markdown", "--content", marker)["document"]
    fetched = call("docs", "+fetch", "--api-version", "v2", "--doc", doc["url"],
                   "--doc-format", "markdown", "--format", "json")["document"]
    assert fetched["title"] == "Probe document" and fetched["content"] == marker, fetched

    space = call("wiki", "+space-create", "--name", "CLI probe space")
    node = call("wiki", "+node-create", "--space-id", space["space_id"], "--obj-type", "docx", "--title", marker)
    resolved = call("wiki", "+node-get", "--node-token", node["node_token"], "--space-id", space["space_id"], "--format", "json")
    assert resolved["obj_token"] == node["obj_token"] and resolved["title"] == marker, resolved
    by_document = call("wiki", "+node-get", "--node-token", node["obj_token"], "--format", "json")
    assert by_document["node_token"] == node["node_token"], by_document
    command("wiki", "+node-get", "--node-token", "wik00000000000000000000000000000000", "--as", "user", success=False)

    fixture = b"Larkbox Drive probe\x00\xff\x10"
    (directory / "upload.bin").write_bytes(fixture)
    uploaded = call("drive", "+upload", "--file", "./upload.bin")
    call("drive", "+download", "--file-token", uploaded["file_token"], "--output", "./downloaded.bin")
    assert (directory / "downloaded.bin").read_bytes() == fixture

    workbook = call("sheets", "+workbook-create", "--title", "Probe workbook", "--format", "json")["spreadsheet"]
    selector = ("--spreadsheet-token", workbook["spreadsheet_token"], "--sheet-name", "Sheet1", "--range", "A1:B2")
    values = [["Task", "Count"], [marker, 23]]
    matrix = [[{"value": value} for value in row] for row in values]
    call("sheets", "+cells-set", *selector, "--cells", json.dumps(matrix), "--format", "json")
    read = call("sheets", "+cells-get", "--url", workbook["url"], *selector[2:], "--format", "json")
    assert read["ranges"][0]["values"] == values, read
    clipped = call("sheets", "+cells-get", *selector, "--format", "json", "--max-chars", "10")
    assert clipped["truncated"] and not clipped["ranges"], clipped

    base = call("base", "+base-create", "--name", "Probe base")["base"]
    selector = ("--base-token", base["base_token"])
    table = call("base", "+table-list", *selector)["tables"][0]
    selector += ("--table-id", table["table_id"])
    schema = call("base", "+field-list", *selector)
    name_field = next(field["field_id"] for field in schema["fields"] if field["field_name"] == "Name")
    call("base", "+record-batch-create", *selector, "--json", json.dumps({"create_records": [{"Name": marker}, {"Name": "Another record"}]}))
    search = ("base", "+record-search", *selector, "--keyword", "sandbox content", "--search-field", name_field)
    found = call(*search, "--format", "json")
    assert found["fields"] == ["Name"] and found["data"] == [[marker]], found
    command(*search, "--output", "./records.ndjson", "--as", "user")
    exported = [json.loads(line) for line in (directory / "records.ndjson").read_text().splitlines()]
    assert [item["Name"] for item in exported] == [marker], exported


def verify_work(command, directory, origin):
    def call(*argv):
        result = json.loads(command(*argv, "--as", "user"))
        assert result["ok"], result
        return result["data"]

    marker = "Scheduling and work probe"
    start, end = "2026-09-01T10:00:00Z", "2026-09-01T10:30:00Z"
    created = call("calendar", "+create", "--summary", marker, "--start", start, "--end", end, "--format", "json")
    selector = ("--event-id", created["event_id"], "--format", "json")
    event = call("calendar", "+get", *selector)
    assert event["summary"] == marker and event["start_time"]["datetime"] == start, event
    agenda = ("calendar", "+agenda", "--start", "2026-09-01T00:00:00Z", "--end", "2026-09-02T00:00:00Z", "--format", "json")
    events = call(*agenda)
    assert [event["event_id"] for event in events] == [created["event_id"]], events
    call("calendar", "+delete", *selector, "--yes")
    assert call(*agenda) == []

    created = call("api", "POST", "/open-apis/task/v2/tasks", "--data", json.dumps({"summary": marker}))["task"]
    task_path = "/open-apis/task/v2/tasks/" + created["guid"]
    assert created["completed_at"] == "0", created
    result = call("task", "+complete", "--task-id", created["guid"], "--format", "json")
    readback = call("api", "GET", task_path)["task"]
    assert readback["completed_at"] == result["completed_at"] and int(result["completed_at"]) > 0, readback
    again = call("task", "+complete", "--task-id", created["guid"], "--format", "json")
    assert again["already_completed"] and again["completed_at"] == result["completed_at"], again
    call("task", "+reopen", "--task-id", created["guid"], "--format", "json")
    assert call("api", "GET", task_path)["task"]["completed_at"] == "0"

    token = "obclarkboxmeeting01"
    transcript = "Alice: The sandbox captured this transcript.\nBob: Confirmed.\n"
    assert seed(origin, minutes=[{"minute_token": token, "title": marker, "owner_open_id": ALICE,
                                  "duration_ms": 60000, "transcript": transcript, "summary": "Probe notes."}])["minutes"] == 1
    result = call("minutes", "+detail", "--minute-tokens", token, "--transcript", "--summary",
                  "--output-dir", "./minutes", "--format", "json")["minutes"][0]
    assert result["title"] == marker and result["artifacts"]["summary"] == "Probe notes.", result
    assert (directory / result["artifacts"]["transcript_file"]).read_text() == transcript, result

    send = ("mail", "+send", "--to", "bob@example.test", "--subject", marker,
            "--body", "The mailbox contains this local test.", "--plain-text", "--no-signature")
    draft = call(*send)
    assert draft["draft_id"] and "message_id" not in draft, draft
    send_draft = ("mail", "+draft-send", "--draft-id", draft["draft_id"], "--yes", "--format", "json")
    sent = call(*send_draft)["sent"][0]
    assert call(*send_draft)["sent"][0]["message_id"] == sent["message_id"]
    found = json.loads(command("mail", "+triage", "--query", marker, "--format", "json", "--as", "user"))
    assert [msg["message_id"] for msg in found["messages"]] == [sent["message_id"]], found
    listed = json.loads(command("mail", "+triage", "--folder", "SENT", "--format", "json", "--as", "user"))
    assert [msg["message_id"] for msg in listed["messages"]] == [sent["message_id"]], listed
    readback = call("mail", "+messages", "--message-ids", sent["message_id"], "--format", "json")
    assert readback["messages"][0]["body_plain_text"].strip() == "The mailbox contains this local test.", readback


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--cli", required=True, type=Path)
    args = parser.parse_args()
    with TemporaryDirectory(prefix="larkbox-cli-") as directory:
        verify(args, Path(directory))


if __name__ == "__main__":
    main()
