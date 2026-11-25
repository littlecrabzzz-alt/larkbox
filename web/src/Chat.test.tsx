import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { SendResult } from "./api";
import { DeliveryStatus } from "./Chat";

type Target = SendResult["deliveries"][number];
const target = (deliveries: Target["deliveries"], error = "", app_id = "cli_bot"): Target =>
  ({ app_id, event_id: "evt_test", deliveries, error });
const status = (...targets: Target[]) => renderToStaticMarkup(<DeliveryStatus targets={targets} />);

describe("message delivery feedback", () => {
  it("reports a locally saved message when target apps have no active transport", () => {
    expect(status(target([]))).toContain("Message saved locally. No active webhook or WebSocket transport.");
  });

  it("reports successful transport names and counts", () => {
    const webhook = { channel: "webhook", status_code: 200, error: "" };
    expect(status(target([
      webhook, { channel: "websocket", status_code: 200, error: "" },
    ]), target([webhook], "", "cli_other"))).toContain("Message saved and delivered 2 via webhook, 1 via WebSocket.");
  });

  it("reports target and transport failures alongside any successful deliveries", () => {
    const html = status(target([
      { channel: "webhook", status_code: 500, error: "receiver error" },
      { channel: "websocket", status_code: 200, error: "" },
    ], "receiver error"), target([], "delivery timed out", "cli_other"));
    expect(html).toContain("Message saved with delivery failures. Delivered 1 via WebSocket.");
    expect(html).toContain("<li>cli_bot (webhook): receiver error</li>");
    expect(html).toContain("<li>cli_other: delivery timed out</li>");
  });
});
