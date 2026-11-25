import com.lark.oapi.Client;
import com.lark.oapi.event.EventDispatcher;
import com.lark.oapi.service.im.ImService;
import com.lark.oapi.service.im.v1.model.CreateMessageReq;
import com.lark.oapi.service.im.v1.model.CreateMessageReqBody;
import com.lark.oapi.service.im.v1.model.CreateMessageResp;
import com.lark.oapi.service.im.v1.model.P2MessageReceiveV1;

public class Probe {
    public static void main(String[] args) throws Exception {
        String domain = args[0];
        String appId = args[1];
        String appSecret = args[2];
        String chatId = args[3];
        String probe = args[4];
        Client rest = new Client.Builder(appId, appSecret).openBaseUrl(domain).build();
        send(rest, chatId, "ready: " + probe);

        EventDispatcher dispatcher = EventDispatcher.newBuilder("", "")
                .onP2MessageReceiveV1(new ImService.P2MessageReceiveV1Handler() {
                    @Override
                    public void handle(P2MessageReceiveV1 event) throws Exception {
                        var message = event.getEvent().getMessage();
                        if (!chatId.equals(message.getChatId()) || !content(probe).equals(message.getContent())) {
                            throw new IllegalStateException("Java SDK received an unexpected message");
                        }
                        send(rest, chatId, "reply: " + probe);
                    }
                }).build();
        var ws = new com.lark.oapi.ws.Client.Builder(appId, appSecret)
                .domain(domain).eventHandler(dispatcher).autoReconnect(false).build();
        ws.start();
        ws.awaitReady(5000);
    }

    private static void send(Client rest, String chatId, String text) throws Exception {
        var body = CreateMessageReqBody.newBuilder()
                .receiveId(chatId).msgType("text").content(content(text)).build();
        CreateMessageResp response = rest.im().v1().message().create(
                CreateMessageReq.newBuilder().receiveIdType("chat_id").createMessageReqBody(body).build());
        if (!response.success() || response.getData().getMessageId() == null) {
            throw new IllegalStateException("Java SDK message.create failed with code " + response.getCode());
        }
    }

    // The driver supplies an ASCII marker containing only letters, spaces and digits.
    private static String content(String text) {
        return "{\"text\":\"" + text + "\"}";
    }
}
