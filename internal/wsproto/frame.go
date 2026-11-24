// Package wsproto encodes Feishu's pbbp2.Frame and its control and data payloads.
package wsproto

import (
	"encoding/json"
	"errors"

	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Frame uses the SDK's generated pure-Go codec, including its required
// zero-valued fields and ordered repeated headers.
type Frame = larkws.Frame

// Header is an ordered key/value pair in a frame.
type Header = larkws.Header

// ClientConfig contains the server's ping and reconnect settings.
type ClientConfig = larkws.ClientConfig

// Response is the SDK's JSON acknowledgement payload.
type Response = larkws.Response

// Frame methods, types, and headers used by the gateway.
const (
	Control = int32(larkws.FrameTypeControl)
	Data    = int32(larkws.FrameTypeData)
	Ping    = string(larkws.MessageTypePing)
	Pong    = string(larkws.MessageTypePong)
	Event   = string(larkws.MessageTypeEvent)
	Card    = string(larkws.MessageTypeCard)

	Type      = larkws.HeaderType
	MessageID = larkws.HeaderMessageID
	TraceID   = larkws.HeaderTraceID
	Sum       = larkws.HeaderSum
	Seq       = larkws.HeaderSeq
	BizRT     = larkws.HeaderBizRt
)

// Encode marshals a pbbp2.Frame.
func Encode(frame *Frame) ([]byte, error) { return frame.Marshal() }

// Decode unmarshals one complete pbbp2.Frame.
func Decode(wire []byte) (Frame, error) {
	var frame Frame
	err := frame.Unmarshal(wire)
	return frame, err
}

// HeaderValue returns the first value for a frame header, as the SDK does.
func HeaderValue(frame Frame, key string) string {
	return larkws.Headers(frame.Headers).GetString(key)
}

// ControlFrame builds a ping or pong. Pong may include a client config.
func ControlFrame(service int32, kind string, config *ClientConfig) (Frame, error) {
	frame := Frame{Service: service, Method: Control, Headers: []Header{{Key: Type, Value: kind}}}
	if config == nil {
		return frame, nil
	}
	var err error
	frame.Payload, err = json.Marshal(config)
	return frame, err
}

// DataFrame builds an unfragmented event or card with correlation headers.
func DataFrame(service int32, kind, messageID, traceID string, payload []byte) Frame {
	return Frame{
		Service: service, Method: Data, Payload: payload,
		Headers: []Header{
			{Key: Type, Value: kind}, {Key: MessageID, Value: messageID},
			{Key: Sum, Value: "1"}, {Key: Seq, Value: "0"}, {Key: TraceID, Value: traceID},
		},
	}
}

// DecodeResponse decodes a client acknowledgement, including base64 data.
func DecodeResponse(payload []byte) (Response, error) {
	var response Response
	if err := json.Unmarshal(payload, &response); err != nil {
		return response, err
	}
	if response.StatusCode < 100 || response.StatusCode > 599 {
		return response, errors.New("invalid websocket acknowledgement status")
	}
	return response, nil
}
