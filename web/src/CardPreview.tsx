import { useState, type ReactNode } from "react";
import { record, type CardAction, type Message } from "./api";
import { Empty } from "./common";

type Node = Record<string, unknown>;
type Act = (action: CardAction) => void;
const list = (value: unknown): unknown[] => Array.isArray(value) ? value : [];
const string = (value: unknown): string => typeof value === "string" ? value : "";
const english = (value: unknown) => {
  const locales = record(value);
  return locales?.en_us ?? locales?.zh_cn ?? Object.values(locales ?? {})[0];
};

export function text(value: unknown) {
  const node = record(value);
  return string(value) || string(node?.content) || string(english(node?.i18n_content ?? node?.i18n));
}

function parse(value: unknown): Node | undefined {
  if (typeof value !== "string") return record(value);
  try {
    return record(JSON.parse(value));
  } catch {
    return undefined;
  }
}

export function messageCard(message: Pick<Message, "raw_card_json" | "rendered_card_json" | "content">) {
  return extractCard(message.raw_card_json) ?? extractCard(message.rendered_card_json) ?? extractCard(message.content);
}

export function extractCard(value: unknown) {
  const raw = parse(value);
  const card = raw?.type === "raw" || raw?.type === "card_json" ? parse(raw.data) : raw;
  if (!card) return;
  const header = record(card.header);
  const elements = record(card.body)?.elements ?? card.elements ?? english(card.i18n_elements);
  if (!header && !Array.isArray(elements)) return;
  return {
    version: JSON.stringify(card),
    title: text(header?.title ?? card.title),
    subtitle: text(header?.subtitle),
    template: string(header?.template),
    elements: list(elements),
  };
}

export function actionFor(node: Node, fields: Partial<Pick<CardAction, "option" | "options" | "input_value" | "form_value">> = {}): CardAction {
  const callback = list(node.behaviors).map(record).find((behavior) => behavior?.type === "callback");
  return {
    tag: string(node.tag),
    value: record(callback?.value) ?? record(node.value),
    name: string(node.name) || undefined,
    ...fields,
  };
}

export function formValues(values: FormData, multipleNames = new Set<string>()) {
  const fields = new Map<string, string | string[]>();
  for (const [name, value] of values) {
    if (typeof value !== "string") continue;
    const previous = fields.get(name);
    fields.set(name, previous === undefined && !multipleNames.has(name) ? value : [previous ?? []].flat().concat(value));
  }
  return Object.fromEntries(fields);
}

export function safeURL(value: unknown) {
  if (typeof value !== "string") return;
  try {
    const url = new URL(value);
    if (url.protocol === "http:" || url.protocol === "https:") return url.href;
  } catch {
    // Invalid URLs remain inert.
  }
}

function Link({ href, children, className }: { href: string; children: ReactNode; className?: string }) {
  return <a href={href} className={className} target="_blank" rel="noopener noreferrer">{children}</a>;
}

// Render a small Markdown subset as React nodes. Raw markup and unsafe links stay text.
export function Markdown({ value }: { value: string }) {
  const tokens = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|\[[^\[\]\n]+\]\([^\s)]+\))/g;
  const parts = value.split(tokens);
  return <span className="card-prose">{parts.map((part, index) => {
    if (index % 2 === 0) return part;
    const link = /^\[([^\]]+)\]\((.+)\)$/.exec(part);
    if (link) {
      const href = safeURL(link[2]);
      return href && !parts[index - 1].endsWith("!")
        ? <Link key={index} href={href}>{link[1]}</Link>
        : part;
    }
    if (part.startsWith("`")) return <code key={index}>{part.slice(1, -1)}</code>;
    if (part.startsWith("**")) return <strong key={index}>{part.slice(2, -2)}</strong>;
    return <em key={index}>{part.slice(1, -1)}</em>;
  })}</span>;
}

function RichText({ value }: { value: unknown }) {
  const tag = record(value)?.tag;
  return tag === "markdown" || tag === "lark_md"
    ? <Markdown value={text(value)} />
    : <span className="card-prose">{text(value)}</span>;
}

const templates: Record<string, string> = {
  blue: "blue", wathet: "blue", indigo: "blue", purple: "purple", violet: "purple", carmine: "red",
  red: "red", orange: "orange", yellow: "orange", green: "green", turquoise: "green", lime: "green",
  grey: "neutral", gray: "neutral",
};

export function CardPreview({ card, onAction }: { card: ReturnType<typeof extractCard>; onAction?: Act }) {
  if (!card) return <Empty title="Card preview unavailable">Inspect the raw JSON for the stored card reference.</Empty>;
  const tone = Object.hasOwn(templates, card.template) ? templates[card.template] : "neutral";
  return <div className="card-preview">
    {(card.title || card.subtitle) && <header className={`card-header card-${tone}`}>
      <strong>{card.title}</strong>{card.subtitle && <small>{card.subtitle}</small>}
    </header>}
    <div className="card-elements"><Elements key={card.version} items={card.elements} act={onAction} /></div>
  </div>;
}

function Elements({ items, act, inForm = false }: { items: unknown[]; act?: Act; inForm?: boolean }) {
  const occurrences = new Map<string, number>();
  return <>{items.map((item, index) => {
    const node = record(item);
    const id = string(node?.element_id);
    const name = string(node?.name);
    const identity = id ? `id:${id}` : name ? `name:${string(node?.tag)}:${name}` : `index:${index}`;
    const count = occurrences.get(identity) ?? 0;
    occurrences.set(identity, count + 1);
    return <Element key={JSON.stringify([identity, count])} item={item} act={act} inForm={inForm} />;
  })}</>;
}

function Element({ item, act, inForm }: { item: unknown; act?: Act; inForm: boolean }) {
  if (Array.isArray(item)) return <div className="card-row"><Elements items={item} act={act} inForm={inForm} /></div>;
  const node = record(item);
  if (!node) return null;
  const children = (items: unknown) => <Elements items={list(items)} act={act} inForm={inForm} />;
  const tag = string(node.tag);
  switch (tag) {
    case "markdown":
    case "lark_md":
    case "plain_text": return <div><RichText value={node} /></div>;
    case "text": return <span className="card-prose">{text(node.text)}</span>;
    case "a": {
      const href = safeURL(node.href);
      return href ? <Link href={href}>{text(node.text)}</Link> : <span>{text(node.text)}</span>;
    }
    case "hr": return <hr />;
    case "div": return <div className="card-elements">
      {node.text != null && <RichText value={node.text} />}
      {list(node.fields).length > 0 && <div className="card-columns">{list(node.fields).map((field, index) =>
        <RichText key={index} value={record(field)?.text} />)}</div>}
      {children([node.extra])}
    </div>;
    case "column_set": return <div className="card-columns">{children(node.columns)}</div>;
    case "column":
    case "interactive_container":
    case "collapsible_panel": return <div className="card-elements">{children(node.elements)}</div>;
    case "note": return <div className="card-note">{children(node.elements)}</div>;
    case "action": return <div className="card-row">{children(node.actions ?? node.elements)}</div>;
    case "img": return <span className="card-note">Image · {text(node.alt) || string(node.img_key ?? node.image_key) || "attachment"}</span>;
    case "form": return inForm
      ? children(node.elements)
      : <form className="card-elements" onSubmit={(event) => event.preventDefault()}>
        <Elements items={list(node.elements)} act={act} inForm />
      </form>;
    case "button": return <CardButton node={node} act={act} inForm={inForm} />;
    case "input": return <CardInput node={node} act={act} inForm={inForm} />;
    case "select_static":
    case "multi_select_static": return <CardSelect node={node} act={act} inForm={inForm} />;
    default: return tag ? <span className="card-note">Component · {tag}</span> : null;
  }
}

function CardButton({ node, act, inForm }: { node: Node; act?: Act; inForm: boolean }) {
  const jump = list(node.behaviors).map(record).find((behavior) => behavior?.type === "open_url");
  const destination = jump?.default_url ?? node.url ?? record(node.multi_url)?.url;
  const href = safeURL(destination);
  const action = actionFor(node);
  const submit = inForm && (node.action_type === "form_submit" || node.form_action_type === "submit");
  const label = text(node.text) || "Run action";
  const className = node.type === "primary" ? "button primary" : node.type === "danger" ? "button danger" : "button";
  if (href && !action.value && !submit && node.disabled !== true) return <Link className={className} href={href}>{label}</Link>;
  return <button className={className} type={submit ? "submit" : "button"}
    disabled={!act || node.disabled === true || (inForm && !submit) || (destination != null && !action.value && !submit)}
    onClick={(event) => {
      event.preventDefault();
      const form = event.currentTarget.form;
      if (submit && form) {
        if (!HTMLFormElement.prototype.reportValidity.call(form)) return;
        const selects = HTMLFormElement.prototype.querySelectorAll.call(form, "select[multiple][name]:enabled");
        const multipleNames = new Set(Array.from(selects, (select) => select.getAttribute("name")!));
        act?.({ ...action, form_value: formValues(new FormData(form), multipleNames) });
      } else {
        act?.(action);
      }
    }}>{label}</button>;
}

function CardInput({ node, act, inForm }: { node: Node; act?: Act; inForm: boolean }) {
  const initial = text(node.default_value);
  const [edit, setEdit] = useState<string>();
  const label = text(node.label) || text(node.placeholder) || string(node.name) || "Card input";
  const input = edit ?? initial;
  const disabled = !act || node.disabled === true;
  return <div className="card-row">
    <label>{label}<input name={string(node.name)} value={input} placeholder={text(node.placeholder)}
      required={node.required === true} disabled={disabled}
      maxLength={typeof node.max_length === "number" && node.max_length > 0 ? node.max_length : undefined}
      onChange={(event) => setEdit(event.target.value)} /></label>
    {!inForm && <button type="button" disabled={disabled} onClick={() => act?.(actionFor(node, { input_value: input }))}>Submit input</button>}
  </div>;
}

function CardSelect({ node, act, inForm }: { node: Node; act?: Act; inForm: boolean }) {
  const multiple = node.tag === "multi_select_static";
  const [selection, setSelection] = useState<string[]>();
  const initial = multiple ? list(node.selected_values ?? node.initial_options).map(string) : [string(node.initial_option)];
  const values = selection ?? initial;
  const label = text(node.label) || text(node.placeholder) || string(node.name) || "Card selection";
  const disabled = !act || node.disabled === true;
  return <div className="card-row"><label>{label}
    <select name={string(node.name)} multiple={multiple} value={multiple ? values : values[0] ?? ""}
      disabled={disabled} required={node.required === true} onChange={(event) => {
        const selected = Array.from(event.target.selectedOptions, (option) => option.value);
        setSelection(selected);
        if (!inForm && !multiple && selected[0]) act?.(actionFor(node, { option: selected[0] }));
      }}>
      {!multiple && <option value="" disabled>Select an option</option>}
      {list(node.options).map((item, index) => {
        const option = record(item);
        return <option key={index} value={string(option?.value)}>{text(option?.text ?? item) || string(option?.value)}</option>;
      })}
    </select>
  </label>{!inForm && multiple && <button type="button" disabled={disabled}
    onClick={() => act?.(actionFor(node, { options: values }))}>Submit selection</button>}</div>;
}
