import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { actionFor, CardPreview, extractCard, formValues, Markdown, messageCard, safeURL } from "./CardPreview";

describe("structured card previews", () => {
  it("uses the current raw CardKit card so the callback data survives the rendered projection", () => {
    const button = { tag: "button", name: "approve", text: { content: "Approve" }, behaviors: [{ type: "callback", value: { ticket: 7 } }] };
    const card = messageCard({
      raw_card_json: { schema: "2.0", header: { template: "green", title: { content: "Review" }, subtitle: { content: "Ticket 7" } },
        body: { elements: [{ tag: "column_set", columns: [{ tag: "column", elements: [button] }] }] } },
      rendered_card_json: { title: "Review", elements: [[{ tag: "button", text: "Approve" }]] },
      content: "{}",
    });
    expect(card).toMatchObject({ title: "Review", subtitle: "Ticket 7", template: "green" });
    expect(actionFor(button)).toEqual({ tag: "button", name: "approve", value: { ticket: 7 } });
    const html = renderToStaticMarkup(<CardPreview card={card} onAction={() => {}} />);
    expect(html).toContain("card-green");
    expect(html).toContain("<button");
    expect(html).toContain(">Approve</button>");
  });

  it("extracts Card JSON 1.0, localized elements and legacy action values", () => {
    const action = { tag: "button", text: { content: "Click" }, value: { queue: "test" } };
    const card = extractCard(JSON.stringify({
      header: { title: { i18n_content: { en_us: "Run" } } },
      i18n_elements: { en_us: [{ tag: "action", actions: [action] }] },
    }));
    expect(card?.title).toBe("Run");
    expect(card?.elements).toEqual([{ tag: "action", actions: [action] }]);
    expect(actionFor(action)).toMatchObject({ tag: "button", value: { queue: "test" } });
  });

  it("constructs form, input and selection callbacks using SDK action fields", () => {
    const data = new FormData();
    data.append("reason", "Looks good");
    data.append("region", "us");
    data.append("region", "eu");
    data.append("__proto__", "literal form key");
    expect(actionFor({ tag: "button", name: "submit" }, { form_value: formValues(data) })).toEqual({
      tag: "button", name: "submit", value: undefined,
      form_value: JSON.parse('{"reason":"Looks good","region":["us","eu"],"__proto__":"literal form key"}'),
    });
    expect(actionFor({ tag: "input" }, { input_value: "Hello" })).toMatchObject({ tag: "input", input_value: "Hello" });
    expect(actionFor({ tag: "select_static" }, { option: "us" })).toMatchObject({ tag: "select_static", option: "us" });
  });

  it("escapes raw markup and keeps untrusted URL schemes inert", () => {
    const html = renderToStaticMarkup(<Markdown value={'<img src=x onerror=alert(1)> [unsafe](javascript:alert) [safe](https://example.test/a) ![image](https://example.test/i) **bold**'} />);
    expect(html).toContain("&lt;img");
    expect(html).not.toContain("<img");
    expect(html).not.toContain('href="javascript:');
    expect(html).toContain('href="https://example.test/a"');
    expect(html).not.toContain('href="https://example.test/i"');
    expect(html).toContain("<strong>bold</strong>");
    expect(safeURL("java\nscript:alert(1)")).toBeUndefined();
    expect(safeURL("data:text/html,<script>")).toBeUndefined();
    expect(safeURL("//example.test")).toBeUndefined();

    const raw = extractCard({ header: { title: { content: "<script>alert(1)</script>" } },
      elements: [{ tag: "button", url: "javascript:alert(1)", text: { content: "Bad link" } }] });
    const card = renderToStaticMarkup(<CardPreview card={raw} onAction={() => {}} />);
    expect(card).not.toContain("<script>");
    expect(card).not.toContain("href=");
    expect(card).toContain('disabled=""');
  });
});
