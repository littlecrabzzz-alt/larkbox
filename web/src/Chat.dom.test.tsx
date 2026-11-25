// @vitest-environment jsdom
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { messagesPath, type Message, type Snapshot } from "./api";
import ChatPlayground from "./Chat";
import { RefreshContext } from "./common";

const snapshot: Snapshot = {
  strict_mode: true,
  apps: [],
  users: [{ open_id: "ou_alice", name: "Alice" }],
  chats: [{ chat_id: "oc_demo", chat_mode: "group", name: "Demo", member_ids: ["ou_alice"] }],
  ws_sessions: [],
};
const card = {
  schema: "2.0",
  body: { elements: [{ tag: "form", name: "review", elements: [
    { tag: "input", name: "reason", default_value: "Initial reason" },
    { tag: "button", name: "save", text: "Save", form_action_type: "submit" },
  ] }] },
};
const message: Message = {
  message_id: "om_card", chat_id: "oc_demo", msg_type: "interactive",
  create_time: "1760000000000", sender: { id: "cli_bot", sender_type: "app" },
  content: JSON.stringify(card), raw_card_json: card, deleted: false, mentions: [], reactions: [],
};
const messages = [message, { ...message, message_id: "om_other" }];

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  container = document.body.appendChild(document.createElement("div"));
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

async function renderChat(callback: () => Response) {
  const fetcher = vi.fn(async (path: string, init: RequestInit) => {
    if (path === messagesPath("oc_demo")) return Response.json({ messages });
    if (path === "/_sandbox/cards/action" && init.method === "POST") return callback();
    throw new Error(`Unexpected request: ${path}`);
  });
  vi.stubGlobal("fetch", fetcher);
  function Chat() {
    const [revision, setRevision] = useState(0);
    return <RefreshContext.Provider value={{ auto: true, revision, refresh: () => setRevision((value) => value + 1) }}>
      <ChatPlayground state={snapshot} />
    </RefreshContext.Provider>;
  }
  await act(async () => root.render(<Chat />));
  return fetcher;
}

function input(index = 0) {
  return container.querySelectorAll<HTMLInputElement>(".card-preview input")[index];
}

function draft(value: string, index = 0) {
  act(() => {
    const field = input(index);
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(field, value);
    field.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

async function submit() {
  await act(async () => container.querySelector<HTMLButtonElement>(".card-preview button")!.click());
}

function actionResponse(applied: boolean, error = "") {
  return Response.json({
    card_applied: applied,
    card_error: error,
    message,
    toast: applied || error ? null : { type: "info", content: "Toast only" },
    delivery: { channel: "webhook", event_id: "evt_card", status_code: error ? 500 : 200, error },
  });
}

describe("chat card drafts", () => {
  it("resets only the submitted card when a callback applies byte-identical JSON", async () => {
    const fetcher = await renderChat(() => actionResponse(true));
    draft("Submitted draft");
    draft("Other card draft", 1);
    const previous = input();
    const other = input(1);

    await submit();

    const action = fetcher.mock.calls.find(([path]) => path === "/_sandbox/cards/action");
    expect(JSON.parse(String(action?.[1].body))).toEqual({
      message_id: "om_card",
      operator_open_id: "ou_alice",
      action: { tag: "button", name: "save", form_value: { reason: "Submitted draft" } },
    });
    expect(container.textContent).toContain("Card update applied");
    expect(input()).not.toBe(previous);
    expect(input().value).toBe("Initial reason");
    expect(input(1)).toBe(other);
    expect(other.value).toBe("Other card draft");
  });

  it("keeps the draft through an unchanged background poll", async () => {
    const fetcher = await renderChat(() => actionResponse(false));
    draft("In progress");
    const previous = input();

    await act(async () => vi.advanceTimersByTimeAsync(2000));

    expect(fetcher.mock.calls.map(([path]) => path)).toEqual([messagesPath("oc_demo"), messagesPath("oc_demo")]);
    expect(input()).toBe(previous);
    expect(input().value).toBe("In progress");
  });

  it.each([
    ["toast-only callback", () => actionResponse(false), "Toast only"],
    ["failed delivery", () => actionResponse(false, "Webhook failed"), "Webhook failed"],
    ["failed request", () => Response.json({ error: "Request rejected" }, { status: 400 }), "Request rejected"],
  ] as const)("keeps the draft after a %s", async (_name, response, notice) => {
    await renderChat(response);
    draft("Keep this draft");
    const previous = input();

    await submit();

    expect(container.textContent).toContain(notice);
    expect(input()).toBe(previous);
    expect(input().value).toBe("Keep this draft");
  });
});
