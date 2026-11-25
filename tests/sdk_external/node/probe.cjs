const lark = require("@larksuiteoapi/node-sdk");

const [domain, appId, appSecret, chatId, probe] = process.argv.slice(2);
const options = { domain, appId, appSecret, loggerLevel: lark.LoggerLevel.error };
const rest = new lark.Client(options);

async function send(text) {
  const result = await rest.im.v1.message.create({
    params: { receive_id_type: "chat_id" },
    data: { receive_id: chatId, msg_type: "text", content: JSON.stringify({ text }) },
  });
  if (result.code !== 0 || !result.data?.message_id) {
    throw new Error("Node.js SDK message.create failed with code " + result.code);
  }
}

async function main() {
  await send("ready: " + probe);
  const ws = new lark.WSClient({ ...options, autoReconnect: false });
  await ws.start({
    eventDispatcher: new lark.EventDispatcher({}).register({
      "im.message.receive_v1": async ({ message }) => {
        if (message?.chat_id !== chatId || JSON.parse(message.content).text !== probe) {
          throw new Error("Node.js SDK received an unexpected message");
        }
        await send("reply: " + probe);
      },
    }),
  });
}

main().catch((err) => {
  console.error(err.message);
  process.exitCode = 1;
});
