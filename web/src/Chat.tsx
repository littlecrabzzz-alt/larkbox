import { useContext, useEffect, useRef, useState, type FormEvent } from "react";
import {
  messagesPath, record, sendMessage, triggerCard, type AppInfo, type CardAction, type Chat,
  type Message, type SendResult, type Snapshot, type User,
} from "./api";
import { CardPreview, messageCard } from "./CardPreview";
import { Badge, Empty, formatTime, Json, Notice, RefreshContext, useOperation, useResource } from "./common";

export default function ChatPlayground({ state }: { state: Snapshot }) {
  const [chatID, setChatID] = useState("");
  const [senderID, setSenderID] = useState("");
  const chat = state.chats.find((item) => item.chat_id === chatID) ?? state.chats[0];
  const users = (state.users ?? []).filter((user) => !user.inactive && chat?.member_ids.includes(user.open_id));
  const sender = users.find((user) => user.open_id === senderID) ?? users[0];
  const bots = state.apps.filter((app) => app.bot_open_id && chat?.member_ids.includes(app.bot_open_id));

  return <div className="workspace chat-workspace">
    <aside className="panel sidebar">
      <div className="panel-heading"><h2>Chats</h2><Badge>{state.chats.length}</Badge></div>
      <div className="item-list" aria-label="Virtual chats">
        {state.chats.length === 0 && <Empty title="No chats" />}
        {state.chats.map((item) => <button className="list-item" type="button" key={item.chat_id}
          aria-pressed={item.chat_id === chat?.chat_id} onClick={() => setChatID(item.chat_id)}>
          <span className={`chat-icon ${item.chat_mode === "p2p" ? "p2p" : ""}`} aria-hidden="true">{item.chat_mode === "p2p" ? "↔" : "#"}</span>
          <span className="item-label">
            <strong>{item.name || (item.chat_mode === "p2p" ? "Direct message" : item.chat_id)}</strong>
            <code title={item.chat_id}>{item.chat_id}</code>
            <span className="item-meta"><Badge>{item.chat_mode}</Badge> {item.member_ids.length} members</span>
          </span>
        </button>)}
      </div>
      <div className="sidebar-section">
        <label htmlFor="sender">Send as user</label>
        <select id="sender" value={sender?.open_id ?? ""} onChange={(event) => setSenderID(event.target.value)} disabled={!sender}>
          {!sender && <option>No active chat members</option>}
          {users.map((user) => <option key={user.open_id} value={user.open_id}>{user.name || user.open_id}</option>)}
        </select>
      </div>
      <div className="sidebar-section">
        <h3>Apps & bots in this chat</h3>
        {bots.length === 0 && <p className="muted">No bots in this chat.</p>}
        {bots.map((bot) => <div className="bot" key={bot.app_id}>
          <div className="bot-heading"><strong>{bot.bot_name || bot.app_id}</strong>
            <Badge tone={bot.active_ws_sessions ? "success" : "neutral"}>{bot.active_ws_sessions} WS</Badge></div>
          <code>{bot.app_id}</code>
          <code>{bot.bot_open_id}</code>
          {bot.webhook_url && <div className="item-meta">Webhook configured</div>}
        </div>)}
      </div>
    </aside>
    {chat
      ? <Conversation key={chat.chat_id} chat={chat} sender={sender} bots={bots} />
      : <section className="panel"><Empty title="No chat selected">Seed a chat to start sending messages.</Empty></section>}
  </div>;
}

function Conversation({ chat, sender, bots }: { chat: Chat; sender?: User; bots: AppInfo[] }) {
  const messages = useResource<{ messages: Message[] }>(messagesPath(chat.chat_id));
  const [reply, setReply] = useState<Message>();
  const composer = useRef<HTMLTextAreaElement>(null);
  const stream = useRef<HTMLDivElement>(null);
  const atBottom = useRef(true);
  const count = messages?.data?.messages.length;
  const activeReply = messages?.data?.messages.find((message) => message.message_id === reply?.message_id && !message.deleted);

  useEffect(() => {
    if (atBottom.current && stream.current) stream.current.scrollTop = stream.current.scrollHeight;
  }, [count]);

  return <section className="panel conversation" aria-label="Conversation">
    <div className="panel-heading chat-heading">
      <div><h2>{chat.name || (chat.chat_mode === "p2p" ? "Direct message" : "Group chat")}</h2><code>{chat.chat_id}</code></div>
      <Badge>{count ?? "—"} {count === 1 ? "message" : "messages"}</Badge>
    </div>
    <div className="stream" ref={stream} onScroll={(event) => {
      const element = event.currentTarget;
      atBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight < 48;
    }}>
      {messages?.error && <Notice tone="error">{messages.error}</Notice>}
      {!messages && <Empty title="Loading messages…" />}
      {count === 0 && <Empty title="No messages yet">Send a message below or use your bot’s Open API client.</Empty>}
      <ol className="messages">
        {messages?.data?.messages.map((message) => <MessageItem key={message.message_id} message={message}
          sender={sender} onReply={() => {
            setReply(message);
            composer.current?.focus();
          }} />)}
      </ol>
    </div>
    <Composer chat={chat} sender={sender} hasBot={bots.length > 0} reply={activeReply}
      cancelReply={() => setReply(undefined)} textarea={composer} />
  </section>;
}

function MessageItem({ message, sender, onReply }: { message: Message; sender?: User; onReply: () => void }) {
  const { refresh } = useContext(RefreshContext);
  const operation = useOperation();
  const [callback, setCallback] = useState<Awaited<ReturnType<typeof triggerCard>>>();
  const isBot = message.sender.sender_type === "app";
  const name = message.sender.sender_name || message.sender.id;
  const toast = callback?.toast;
  const toastTone = toast?.type && ["success", "warning", "error"].includes(toast.type) ? toast.type : "info";
  const toastText = toast?.content || toast?.i18n?.en_us || Object.values(toast?.i18n ?? {})[0];

  const runAction = sender && !operation.busy ? (action: CardAction) => void operation.run(async () => {
    setCallback(undefined);
    const result = await triggerCard(message.message_id, sender.open_id, action);
    setCallback(result);
    refresh();
  }) : undefined;

  return <li className="message">
    <div className={`avatar ${isBot ? "bot-avatar" : ""}`} aria-hidden="true">{name.trim().slice(0, 1).toUpperCase() || "?"}</div>
    <article className="message-body">
      <header className="message-heading">
        <strong>{name}</strong><Badge tone={isBot ? "info" : "neutral"}>{isBot ? "Bot" : "User"}</Badge>
        {message.deleted && <Badge tone="warning">Recalled</Badge>}
        <time dateTime={new Date(Number(message.create_time)).toISOString()}>{formatTime(message.create_time, true)}</time>
      </header>
      {(message.parent_id || message.root_id || message.thread_id) && <div className="thread-meta">
        {message.parent_id && <span>Reply to <code>{message.parent_id}</code></span>}
        {message.root_id && <span>Root <code>{message.root_id}</code></span>}
        {message.thread_id && <span>Thread <code>{message.thread_id}</code></span>}
      </div>}
      {message.deleted ? <p className="muted">This message was recalled.</p>
        : message.msg_type === "interactive" ? <div className="message-card">
          {message.card_id && <div className="item-meta">CardKit <code>{message.card_id}</code></div>}
          <CardPreview card={messageCard(message)} onAction={runAction} />
          {operation.busy && <Notice>Waiting for card callback…</Notice>}
          {operation.error && <Notice tone="error">{operation.error}</Notice>}
          {toastText && <Notice tone={toastTone}>{toastText}</Notice>}
          {callback && <div className="callback">
            {callback.card_error && <Notice tone="error">{callback.card_error}</Notice>}
            {callback.card_applied && <div className="item-meta">Card update applied</div>}
            <details><summary>Callback delivery</summary><Json value={callback.delivery} /></details>
          </div>}
        </div> : <MessageContent message={message} />}
      {message.reactions.length > 0 && <div className="message-tools">
        {message.reactions.map((reaction) => <span className="badge" key={reaction.reaction_id}
          title={reaction.operator_open_id || reaction.operator_app_id}>{reaction.emoji_type}</span>)}
      </div>}
      <div className="message-tools">
        {!message.deleted && <button className="text-button" type="button" onClick={onReply}>Reply</button>}
        <details><summary>Raw JSON</summary><Json value={message} /></details>
      </div>
    </article>
  </li>;
}

function MessageContent({ message }: { message: Message }) {
  let payload: unknown;
  try {
    payload = JSON.parse(message.content);
  } catch {
    payload = message.content;
  }
  const content = record(payload);
  if (message.msg_type === "text" && typeof content?.text === "string") {
    const mentions = new Map(message.mentions.map((mention) => [mention.key, mention.name || mention.id]));
    const text = content.text.replace(/@_user_\d+/g, (key) => mentions.has(key) ? `@${mentions.get(key)}` : key);
    return <p className="message-text">{text}</p>;
  }
  if (["image", "file", "audio", "media", "sticker"].includes(message.msg_type)) {
    const key = content?.file_name ?? content?.image_key ?? content?.file_key;
    return <div className="attachment"><Badge tone="info">{message.msg_type}</Badge> <code>{typeof key === "string" ? key : "Attachment"}</code></div>;
  }
  return <div><Badge>{message.msg_type}</Badge><Json value={payload} /></div>;
}

function Composer({ chat, sender, hasBot, reply, cancelReply, textarea }: {
  chat: Chat; sender?: User; hasBot: boolean; reply?: Message; cancelReply: () => void;
  textarea: React.RefObject<HTMLTextAreaElement | null>;
}) {
  const { refresh } = useContext(RefreshContext);
  const [mode, setMode] = useState("text");
  const [type, setType] = useState("post");
  const [draft, setDraft] = useState("");
  const [mention, setMention] = useState(false);
  const [thread, setThread] = useState(false);
  const [result, setResult] = useState<SendResult>();
  const operation = useOperation();
  const canMention = hasBot && (mode === "text" || ["text", "post"].includes(type.trim()));

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!sender || !draft.trim()) return;
    void operation.run(async () => {
      setResult(undefined);
      const content = mode === "text" ? { text: draft } : {
        msg_type: type.trim(),
        content: JSON.stringify(JSON.parse(draft)),
      };
      const next = await sendMessage({
        chat_id: chat.chat_id, sender_open_id: sender.open_id, at_bot: canMention && mention,
        parent_id: reply?.message_id, reply_in_thread: reply ? thread : undefined,
        ...content,
      });
      setResult(next);
      setDraft("");
      setThread(false);
      cancelReply();
      refresh();
    });
  }

  return <form className="composer" onSubmit={submit}>
    {reply && <div className="reply-banner">
      <span>Replying to <strong>{reply.sender.sender_name || reply.sender.id}</strong> <code>{reply.message_id}</code></span>
      <label className="check"><input type="checkbox" checked={thread} onChange={(event) => setThread(event.target.checked)} /> Reply in thread</label>
      <button type="button" className="text-button" onClick={() => { cancelReply(); setThread(false); }}>Cancel reply</button>
    </div>}
    <div className="composer-heading">
      <label className="inline-label">Format
        <select value={mode} onChange={(event) => setMode(event.target.value)}>
          <option value="text">Text</option><option value="json">Custom JSON</option>
        </select>
      </label>
      {mode === "json" && <label className="inline-label">Message type
        <input value={type} onChange={(event) => setType(event.target.value)} required />
      </label>}
      <label className="check"><input type="checkbox" checked={canMention && mention} disabled={!canMention}
        onChange={(event) => setMention(event.target.checked)} /> @bot</label>
    </div>
    <label htmlFor="composer-draft" className="sr-only">{mode === "text" ? "Message" : "JSON content"}</label>
    <textarea id="composer-draft" ref={textarea} rows={3} value={draft} disabled={operation.busy}
      placeholder={mode === "text" ? "Write a message…" : "Enter the message content as JSON…"}
      onChange={(event) => setDraft(event.target.value)}
      onKeyDown={(event) => { if ((event.metaKey || event.ctrlKey) && event.key === "Enter") event.currentTarget.form?.requestSubmit(); }} />
    <div className="composer-footer">
      <span className="muted">{sender ? `Sending as ${sender.name || sender.open_id}` : "Choose an active chat member to send."}</span>
      <button className="primary" type="submit" disabled={!sender || !draft.trim() || operation.busy}>{operation.busy ? "Sending…" : "Send message"}</button>
    </div>
    {operation.error && <Notice tone="error">{operation.error}</Notice>}
    {result && <DeliveryStatus targets={result.deliveries} />}
  </form>;
}

function channelName(channel: string) {
  if (channel === "websocket") return "WebSocket";
  if (channel === "webhook") return "webhook";
  return channel || "unknown transport";
}

export function DeliveryStatus({ targets }: { targets: SendResult["deliveries"] }) {
  const channels = new Map<string, number>();
  const failures: string[] = [];
  for (const target of targets) {
    const previousFailures = failures.length;
    for (const delivery of target.deliveries) {
      const channel = channelName(delivery.channel);
      if (delivery.error || delivery.status_code < 200 || delivery.status_code >= 300) {
        failures.push(`${target.app_id} (${channel}): ${delivery.error || (delivery.status_code ? `status ${delivery.status_code}` : "no response")}`);
      } else {
        channels.set(channel, (channels.get(channel) ?? 0) + 1);
      }
    }
    if (target.error && previousFailures === failures.length) failures.push(`${target.app_id}: ${target.error}`);
  }

  const delivered = [...channels].map(([channel, count]) => `${count} via ${channel}`).join(", ");
  const outcome = failures.length
    ? `Message saved with delivery failures.${delivered ? ` Delivered ${delivered}.` : ""}`
    : delivered ? `Message saved and delivered ${delivered}.` : "Message saved locally. No active webhook or WebSocket transport.";
  return <Notice tone={failures.length ? "warning" : delivered ? "success" : "info"}>
    {outcome}
    {failures.length > 0 && <ul>{failures.map((failure, index) => <li key={index}>{failure}</li>)}</ul>}
    {targets.length > 0 && <details><summary>Delivery results</summary><Json value={targets} /></details>}
  </Notice>;
}
