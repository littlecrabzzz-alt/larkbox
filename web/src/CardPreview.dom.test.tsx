// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CardPreview, extractCard } from "./CardPreview";

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  container = document.body.appendChild(document.createElement("div"));
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
});

function formCard(submitField: object, reason = "Initial reason", selected = ["eu"]) {
  return {
    schema: "2.0",
    body: { elements: [{ tag: "form", name: "review", elements: [
      { tag: "input", name: "reason", default_value: reason, required: true },
      {
        tag: "multi_select_static", name: "regions", element_id: "regions_picker",
        selected_values: selected, initial_options: ["us"],
        options: [{ value: "us" }, { value: "eu" }],
      },
      { tag: "button", name: "save", text: "Save", value: { review_id: 7 }, ...submitField },
    ] }] },
  };
}

describe("card form callbacks", () => {
  it("preserves checkbox booleans and date offsets in a form submission", () => {
    const onAction = vi.fn();
    const card = extractCard({ schema: "2.0", body: { elements: [{
      tag: "form", name: "schedule", elements: [
        { tag: "checker", name: "urgent", checked: false, text: { tag: "plain_text", content: "Urgent" } },
        { tag: "date_picker", name: "when", initial_date: "2026-04-09" },
        { tag: "button", name: "save", form_action_type: "submit", text: { tag: "plain_text", content: "Save" } },
      ],
    }] } });
    act(() => root.render(<CardPreview card={card} onAction={onAction} />));
    const submit = container.querySelector<HTMLButtonElement>("button[type=submit]")!;
    act(() => submit.click());
    expect(onAction.mock.calls[0][0].form_value).toEqual({ urgent: false, when: expect.stringMatching(/^2026-04-09 [+-]\d{4}$/) });
    act(() => container.querySelector<HTMLInputElement>("input[type=checkbox]")!.click());
    act(() => submit.click());
    expect(onAction.mock.calls[1][0].form_value.urgent).toBe(true);
  });

  it.each([{ action_type: "form_submit" }, { form_action_type: "submit" }])(
    "submits the form with %o and includes selected_values",
    (submitField) => {
      const onAction = vi.fn();
      act(() => root.render(<CardPreview card={extractCard(formCard(submitField))} onAction={onAction} />));
      const select = container.querySelector("select")!;
      expect([...select.selectedOptions].map((option) => option.value)).toEqual(["eu"]);
      const button = container.querySelector("button")!;
      expect(button.type).toBe("submit");
      expect(button.disabled).toBe(false);
      act(() => button.click());
      expect(onAction).toHaveBeenCalledExactlyOnceWith({
        tag: "button", name: "save", value: { review_id: 7 },
        form_value: { reason: "Initial reason", regions: ["eu"] },
      });
    },
  );

  it("uses legacy initial_options for standalone multi-select callbacks", () => {
    const onAction = vi.fn();
    const picker = {
      tag: "multi_select_static", name: "regions", initial_options: ["us", "eu"],
      options: [{ value: "us" }, { value: "eu" }], value: { review_id: 7 },
    };
    act(() => root.render(<CardPreview card={extractCard({ elements: [picker] })} onAction={onAction} />));
    act(() => container.querySelector("button")!.click());
    expect(onAction).toHaveBeenCalledExactlyOnceWith({
      tag: "multi_select_static", name: "regions", value: { review_id: 7 }, options: ["us", "eu"],
    });
  });

  it("keeps field drafts across unchanged polling responses and resets them to new card defaults", () => {
    const submit = { form_action_type: "submit" };
    const onAction = vi.fn();
    const render = (card: object) => act(() => root.render(<CardPreview card={extractCard(card)} onAction={onAction} />));
    render(formCard(submit));
    const input = container.querySelector("input")!;
    const select = container.querySelector("select")!;

    act(() => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, "Local draft");
      input.dispatchEvent(new Event("input", { bubbles: true }));
      select.options[0].selected = true;
      select.dispatchEvent(new Event("change", { bubbles: true }));
    });

    render(JSON.parse(JSON.stringify(formCard(submit))));
    expect(container.querySelector("input")).toBe(input);
    expect(input.value).toBe("Local draft");
    expect([...select.selectedOptions].map((option) => option.value)).toEqual(["us", "eu"]);
    act(() => container.querySelector("button")!.click());
    expect(onAction).toHaveBeenLastCalledWith({
      tag: "button", name: "save", value: { review_id: 7 },
      form_value: { reason: "Local draft", regions: ["us", "eu"] },
    });

    render(formCard(submit, "Updated reason", ["us"]));
    expect(container.querySelector("input")!.value).toBe("Updated reason");
    expect([...container.querySelector("select")!.selectedOptions].map((option) => option.value)).toEqual(["us"]);
    act(() => container.querySelector("button")!.click());
    expect(onAction).toHaveBeenLastCalledWith({
      tag: "button", name: "save", value: { review_id: 7 },
      form_value: { reason: "Updated reason", regions: ["us"] },
    });
  });
});
