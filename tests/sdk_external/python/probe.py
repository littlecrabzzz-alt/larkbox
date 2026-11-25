import json
import sys

import lark_oapi as lark
from lark_oapi.api.im.v1 import CreateMessageRequest, CreateMessageRequestBody


def main():
    domain, app_id, app_secret, chat_id, probe = sys.argv[1:]
    rest = lark.Client.builder().domain(domain).app_id(app_id).app_secret(app_secret).build()

    def send(text):
        body = (
            CreateMessageRequestBody.builder().receive_id(chat_id).msg_type("text")
            .content(json.dumps({"text": text})).build()
        )
        request = CreateMessageRequest.builder().receive_id_type("chat_id").request_body(body).build()
        response = rest.im.v1.message.create(request)
        if not response.success() or not response.data.message_id:
            raise RuntimeError(f"Python SDK message.create failed with code {response.code}")

    def receive(event):
        message = event.event.message
        if message.chat_id != chat_id or json.loads(message.content)["text"] != probe:
            raise RuntimeError("Python SDK received an unexpected message")
        send("reply: " + probe)

    send("ready: " + probe)
    handler = lark.EventDispatcherHandler.builder("", "").register_p2_im_message_receive_v1(receive).build()
    lark.ws.Client(app_id, app_secret, domain=domain, event_handler=handler, auto_reconnect=False).start()


if __name__ == "__main__":
    main()
