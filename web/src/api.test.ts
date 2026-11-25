import { afterEach, describe, expect, it, vi } from "vitest";
import {
  addFault, cardPath, expireTokens, faultPath, messagesPath, request, resetSandbox, sendMessage, tracesPath,
  triggerCard, verifyWebhook,
} from "./api";

afterEach(() => vi.unstubAllGlobals());

describe("sandbox API", () => {
  it("encodes entity IDs and skips empty trace filters", () => {
    expect(messagesPath("chat/a#")).toBe("/_sandbox/chats/chat%2Fa%23/messages");
    expect(cardPath("card/a")).toBe("/_sandbox/cards/card%2Fa");
    expect(faultPath("fault/a")).toBe("/_sandbox/faults/fault%2Fa");
    expect(tracesPath({ direction: "inbound", path_prefix: "/open-apis/im/", app_id: " " }))
      .toBe("/_sandbox/traces?direction=inbound&path_prefix=%2Fopen-apis%2Fim%2F");
  });

  it("posts JSON to the exact control-plane paths and preserves wire field names", async () => {
    const fetcher = vi.fn(async (_url: string, _init: RequestInit) => new Response("{}"));
    vi.stubGlobal("fetch", fetcher);
    await sendMessage({
      chat_id: "oc_1", sender_open_id: "ou_1", at_bot: true, parent_id: "om_parent",
      reply_in_thread: true, msg_type: "post", content: '{"en_us":{"content":[]}}',
    });
    await triggerCard("om_1", "ou_1", { tag: "button", name: "confirm", value: { id: 7 } });
    await addFault({ kind: "latency", delay_ms: 250, method: "POST", remaining_hits: 2 });
    await verifyWebhook("cli_1");
    await expireTokens("cli_1");
    await resetSandbox();
    expect(fetcher.mock.calls.map(([url, init]) => [url, JSON.parse(String(init.body))])).toEqual([
      ["/_sandbox/messages/send", {
        chat_id: "oc_1", sender_open_id: "ou_1", at_bot: true, parent_id: "om_parent",
        reply_in_thread: true, msg_type: "post", content: '{"en_us":{"content":[]}}',
      }],
      ["/_sandbox/cards/action", {
        message_id: "om_1", operator_open_id: "ou_1",
        action: { tag: "button", name: "confirm", value: { id: 7 } },
      }],
      ["/_sandbox/faults", { kind: "latency", delay_ms: 250, method: "POST", remaining_hits: 2 }],
      ["/_sandbox/events/webhook/verify", { app_id: "cli_1" }],
      ["/_sandbox/tokens/expire", { app_id: "cli_1" }],
      ["/_sandbox/reset", {}],
    ]);
    expect(fetcher.mock.calls[0][1]).toMatchObject({
      method: "POST", mode: "same-origin", cache: "no-store",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
    });
  });

  it("surfaces JSON request errors and rejects HTML success pages", async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(new Response('{"error":"unknown sandbox endpoint"}', { status: 404 }))
      .mockResolvedValueOnce(new Response("<h1>SPA</h1>"));
    vi.stubGlobal("fetch", fetcher);
    await expect(request("/_sandbox/missing")).rejects.toThrow("unknown sandbox endpoint");
    await expect(request("/_sandbox/state")).rejects.toThrow("Expected JSON from /_sandbox/state");
  });
});
