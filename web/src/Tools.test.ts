import { describe, expect, it } from "vitest";
import type { FaultKind } from "./api";
import { buildFaultSpec } from "./Tools";

function payload(kind: FaultKind, fields: Record<string, string>) {
  const data = new FormData();
  for (const [name, value] of Object.entries(fields)) data.set(name, value);
  return JSON.parse(JSON.stringify(buildFaultSpec(kind, data)));
}

describe("fault rule payloads", () => {
  it("omits blank optional fields and preserves server defaults", () => {
    expect(payload("rate_limit", { retry_after: "", probability: "", remaining_hits: "" })).toEqual({ kind: "rate_limit" });
    expect(payload("http_error", { error_code: "", error_msg: "  ", status_code: "" })).toEqual({ kind: "http_error" });
  });

  it("sends explicit zero retries, probability and custom HTTP error fields", () => {
    expect(payload("rate_limit", { retry_after: "0", probability: "0.25", method: "get" })).toEqual({
      kind: "rate_limit", retry_after: 0, probability: 0.25, method: "GET",
    });
    expect(payload("http_error", { error_code: "99999001", error_msg: " Injected error ", status_code: "503" })).toEqual({
      kind: "http_error", error_code: 99999001, error_msg: "Injected error", status_code: 503,
    });
  });
});
