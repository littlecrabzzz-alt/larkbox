import { useState, type ChangeEvent, type ReactNode } from "react";
import { cardImagePath, record, type CardAction, type Message } from "./api";
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
    tags: list(english(header?.i18n_text_tag_list) ?? header?.text_tag_list).slice(0, 3),
    elements: list(elements),
  };
}

export function actionFor(node: Node, fields: Partial<Pick<CardAction, "option" | "options" | "input_value" | "checked" | "timezone" | "form_value">> = {}): CardAction {
  const callback = list(node.behaviors).map(record).find((behavior) => behavior?.type === "callback");
  return {
    tag: string(node.tag),
    value: record(callback?.value) ?? record(node.value),
    name: string(node.name) || undefined,
    ...fields,
  };
}

export function formValues(values: FormData, multipleNames = new Set<string>(), checks = new Map<string, boolean>()) {
  const fields = new Map<string, string | string[]>();
  for (const [name, value] of values) {
    if (typeof value !== "string" || checks.has(name)) continue;
    const previous = fields.get(name);
    fields.set(name, previous === undefined && !multipleNames.has(name) ? value : [previous ?? []].flat().concat(value));
  }
  return Object.fromEntries<string | string[] | boolean>([...fields, ...checks]);
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

export function CardPreview({ card, onAction, generation = 0 }: {
  card: ReturnType<typeof extractCard>;
  onAction?: Act;
  generation?: number;
}) {
  if (!card) return <Empty title="Card preview unavailable">Inspect the raw JSON for the stored card reference.</Empty>;
  const tone = Object.hasOwn(templates, card.template) ? templates[card.template] : "neutral";
  return <div className="card-preview">
    {(card.title || card.subtitle || card.tags.length > 0) && <header className={`card-header card-${tone}`}>
      <div className="card-row">
        {card.title && <strong>{card.title}</strong>}
        {card.tags.map((item, index) => <CardTag key={index} node={record(item) ?? {}} />)}
      </div>
      {card.subtitle && <small>{card.subtitle}</small>}
    </header>}
    <div className="card-elements"><Elements key={`${generation}:${card.version}`} items={card.elements} act={onAction} /></div>
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
    case "column": return <div className="card-elements">{children(node.elements)}</div>;
    case "interactive_container": {
      const callback = list(node.behaviors).map(record).find((behavior) => behavior?.type === "callback");
      const link = list(node.behaviors).map(record).find((behavior) => behavior?.type === "open_url");
      const href = safeURL(link?.default_url);
      return <div className="card-elements">
        {children(node.elements)}
        {callback && <button type="button" disabled={!act || node.disabled === true}
          onClick={() => act?.(actionFor(node))}>Run container action</button>}
        {href && <Link href={href}>Open link</Link>}
      </div>;
    }
    case "collapsible_panel": return <CardPanel node={node}>{children(node.elements)}</CardPanel>;
    case "note": return <div className="card-note">{children(node.elements)}</div>;
    case "action": return <div className="card-row">{children(node.actions ?? node.elements)}</div>;
    case "text_tag": return <CardTag node={node} />;
    case "img": return <CardImage node={node} />;
    case "img_combination": return <div className="card-image-grid">{list(node.img_list).map((image, i) =>
      <CardImage key={i} node={record(image) ?? {}} />)}</div>;
    case "person": return <span className="card-note">{string(node.user_id)}</span>;
    case "person_list": return <ul className="card-people">{list(node.persons).map((person, i) =>
      <li key={i}>{string(record(person)?.id)}</li>)}</ul>;
    case "table": return <CardTable node={node} />;
    case "chart": return <CardChart node={node} />;
    case "form": return inForm
      ? children(node.elements)
      : <form className="card-elements" onSubmit={(event) => event.preventDefault()}>
        <Elements items={list(node.elements)} act={act} inForm />
      </form>;
    case "button": return <CardButton node={node} act={act} inForm={inForm} />;
    case "input": return <CardInput node={node} act={act} inForm={inForm} />;
    case "checker": return <CardChecker node={node} act={act} inForm={inForm} />;
    case "date_picker":
    case "picker_datetime":
    case "picker_time": return <CardPicker node={node} act={act} inForm={inForm} />;
    case "select_person":
    case "multi_select_person":
    case "select_img":
    case "overflow":
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
        const checkboxes = HTMLFormElement.prototype.querySelectorAll.call(form, "input[type=checkbox][name]:enabled");
        const checks = new Map(Array.from(checkboxes, (input) => [(input as HTMLInputElement).name, (input as HTMLInputElement).checked]));
        act?.({ ...action, form_value: formValues(new FormData(form), multipleNames, checks) });
      } else {
        act?.(action);
      }
    }}>{label}</button>;
}

type CardControlProps = { node: Node; act?: Act; inForm: boolean };

function CardInput({ node, act, inForm }: CardControlProps) {
  const initial = text(node.default_value);
  const [edit, setEdit] = useState<string>();
  const label = text(node.label) || text(node.placeholder) || string(node.name) || "Card input";
  const input = edit ?? initial;
  const disabled = !act || node.disabled === true;
  const common = {
    name: string(node.name), value: input, placeholder: text(node.placeholder),
    required: node.required === true, disabled,
    maxLength: typeof node.max_length === "number" && node.max_length > 0 ? node.max_length : undefined,
    onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => setEdit(event.target.value),
  };
  return <div className="card-row">
    <label>{label}{node.input_type === "multiline_text" ? <textarea {...common}
      rows={typeof node.rows === "number" && node.rows >= 1 ? node.rows : undefined} /> :
      <input {...common} type={node.input_type === "password" ? "password" : "text"} />}</label>
    {!inForm && <button type="button" disabled={disabled} onClick={() => act?.(actionFor(node, { input_value: input }))}>Submit input</button>}
  </div>;
}

function CardSelect({ node, act, inForm }: CardControlProps) {
  const multiple = node.tag === "multi_select_static" || node.tag === "multi_select_person";
  const [selection, setSelection] = useState<string[]>();
  const initial = multiple ? list(node.selected_values ?? node.initial_options).map(string) : [string(node.initial_option)];
  const values = selection ?? initial;
  const label = text(node.label) || text(node.placeholder) || string(node.name) || "Card selection";
  const options = list(node.options);
  const disabled = !act || node.disabled === true || options.length === 0;
  return <div className="card-row"><label>{label}
    <select name={string(node.name)} multiple={multiple} value={multiple ? values : values[0] ?? ""}
      disabled={disabled} required={node.required === true} onChange={(event) => {
        const selected = Array.from(event.target.selectedOptions, (option) => option.value);
        setSelection(selected);
        if (!inForm && !multiple && selected[0]) act?.(actionFor(node, { option: selected[0] }));
      }}>
      {!multiple && <option value="" disabled>Select an option</option>}
      {options.map((item, index) => {
        const option = record(item);
        const value = string(option?.value ?? option?.img_key);
        return <option key={index} value={value}>{text(option?.text ?? option?.alt ?? item) || value}</option>;
      })}
    </select>
  </label>{!inForm && multiple && <button type="button" disabled={disabled}
    onClick={() => act?.(actionFor(node, { options: values }))}>Submit selection</button>}</div>;
}

function CardTag({ node }: { node: Node }) {
  const value = text(node.text);
  const color = string(node.color).split("_")[0];
  const tone = Object.hasOwn(templates, color) ? templates[color] : "neutral";
  return value && <span className={`badge card-${tone}`}>{value}</span>;
}

function CardPanel({ node, children }: { node: Node; children: ReactNode }) {
  const header = record(node.header);
  const title = text(header?.title ?? node.title);
  return <details className="card-panel" open={node.expanded === true}>
    <summary>{title || "Details"}</summary>
    <div className="card-elements">{children}</div>
  </details>;
}

function CardImage({ node }: { node: Node }) {
  const key = string(node.img_key ?? node.image_key);
  const alt = text(node.alt) || "Card image";
  const [failed, setFailed] = useState(false);
  return key && !failed
    ? <img className="card-image" alt={alt} src={cardImagePath(key)} loading="lazy" onError={() => setFailed(true)} />
    : <span className="card-note">{alt}</span>;
}

function CardTable({ node }: { node: Node }) {
  const columns = list(node.columns).map(record).flatMap((column) => {
    const key = string(column?.name);
    return key ? [{ key, label: string(column?.display_name) || key }] : [];
  });
  return <CardRows rows={list(node.rows).map(record)} columns={columns} />;
}

function CardChart({ node }: { node: Node }) {
  const spec = record(node.chart_spec);
  const datasets = Array.isArray(spec?.data) ? spec.data : [spec?.data];
  const rows = datasets.flatMap((data) => list(record(data)?.values)).map(record);
  const columns = [...new Set(rows.flatMap((row) => Object.keys(row ?? {})))].map((key) => ({ key, label: key }));
  return <figure className="card-chart">
    <figcaption>{text(record(spec?.title)?.text) || `Chart · ${string(spec?.type) || "data"}`}</figcaption>
    <CardRows rows={rows} columns={columns} />
  </figure>;
}

function CardRows({ rows, columns }: { rows: (Node | undefined)[]; columns: { key: string; label: string }[] }) {
  if (!columns.length) return null;
  return <div className="table-wrap"><table className="card-table">
    <thead><tr>{columns.map(({ key, label }) => <th key={key} scope="col">{label}</th>)}</tr></thead>
    <tbody>{rows.map((row, i) => <tr key={i}>{columns.map(({ key }) => {
      const cell = row && Object.hasOwn(row, key) ? row[key] : null;
      return <td key={key}>{cell == null ? "" : typeof cell === "object" ? JSON.stringify(cell) : String(cell)}</td>;
    })}</tr>)}</tbody>
  </table></div>;
}

function CardChecker({ node, act, inForm }: CardControlProps) {
  const [checked, setChecked] = useState(node.checked === true);
  return <label className="check">
    <input type="checkbox" name={string(node.name)} checked={checked}
      disabled={!act || node.disabled === true} required={node.required === true}
      onChange={(event) => {
        const next = event.currentTarget.checked;
        setChecked(next);
        if (!inForm) act?.(actionFor(node, { checked: next }));
      }} />
    <RichText value={node.text ?? (string(node.name) || "Checkbox")} />
  </label>;
}

const pickerTypes = {
  date_picker: { type: "date", initial: "initial_date", label: "Date" },
  picker_datetime: { type: "datetime-local", initial: "initial_datetime", label: "Date and time" },
  picker_time: { type: "time", initial: "initial_time", label: "Time" },
} as const;

function CardPicker({ node, act, inForm }: CardControlProps) {
  const picker = pickerTypes[node.tag as keyof typeof pickerTypes];
  const [current, setCurrent] = useState(string(node[picker.initial]).replace(" ", "T"));
  const disabled = !act || node.disabled === true;
  const label = text(node.label ?? node.placeholder ?? node.text) || string(node.name) || picker.label;
  const value = current ? pickerValue(current, picker.type) : "";
  return <div className="card-row">
    <label>{label}<input type={picker.type} value={current}
      disabled={disabled} required={node.required === true}
      onChange={(event) => setCurrent(event.target.value)} /></label>
    {inForm
      ? <input type="hidden" name={string(node.name)} value={value} disabled={disabled} />
      : <button type="button" disabled={disabled || !value} onClick={() => act?.(actionFor(node, {
        option: value, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      }))}>Confirm {picker.label.toLowerCase()}</button>}
  </div>;
}

function pickerValue(value: string, type: string) {
  const date = type === "time" ? new Date() : new Date(type === "date" ? value + "T00:00" : value);
  if (type === "time") {
    const [hours, minutes] = value.split(":").map(Number);
    date.setHours(hours, minutes);
  }
  const offset = -date.getTimezoneOffset();
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${value.replace("T", " ")} ${offset < 0 ? "-" : "+"}${pad(Math.floor(Math.abs(offset) / 60))}${pad(Math.abs(offset) % 60)}`;
}
