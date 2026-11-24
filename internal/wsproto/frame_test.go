package wsproto_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/gogo/protobuf/proto"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/wsproto"
)

func TestFrameWireMatchesSDK(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame larkws.Frame
	}{
		{"zero", larkws.Frame{}},
		{"empty payload", larkws.Frame{Payload: []byte{}}},
		{"ping", *larkws.NewPingFrame(123)},
		{"all fields", larkws.Frame{
			SeqID: math.MaxUint64, LogID: 1234567, Service: -1, Method: 1,
			Headers:         []larkws.Header{{Key: "type", Value: "event"}, {Key: "type", Value: "card"}, {Key: "", Value: ""}},
			PayloadEncoding: "gzip", PayloadType: "application/json", Payload: []byte{0, 0xff}, LogIDNew: "log-123",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := proto.Marshal(&tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := wsproto.Encode(&tc.frame)
			if err != nil || !bytes.Equal(wire, want) {
				t.Fatalf("encoded %x, %v, SDK %x", wire, err, want)
			}
			if tc.name == "zero" && hex.EncodeToString(wire) != "080010001800200032003a004a00" {
				t.Fatalf("zero frame field tags changed: %x", wire)
			}
			var official larkws.Frame
			if err := proto.Unmarshal(wire, &official); err != nil || !reflect.DeepEqual(official, tc.frame) {
				t.Fatalf("SDK decoded %#v, %v", official, err)
			}
			got, err := wsproto.Decode(want)
			if err != nil || !reflect.DeepEqual(got, tc.frame) {
				t.Fatalf("decoded %#v, %v", got, err)
			}
		})
	}
}

func TestFrameFieldTags(t *testing.T) {
	frame := wsproto.Frame{
		SeqID: 1, LogID: 2, Service: 3, Method: 1,
		Headers:         []wsproto.Header{{Key: "k", Value: "v"}, {Key: "k", Value: "w"}},
		PayloadEncoding: "e", PayloadType: "t", Payload: []byte("p"), LogIDNew: "l",
	}
	wire, err := wsproto.Encode(&frame)
	const golden = "08011002180320012a060a016b1201762a060a016b1201773201653a01744201704a016c"
	if err != nil || hex.EncodeToString(wire) != golden {
		t.Fatalf("encoded %x, %v, want %s", wire, err, golden)
	}
	// Unknown fields are skipped. Duplicated headers retain their wire order.
	wire = append(wire, 0x50, 0x01)
	got, err := wsproto.Decode(wire)
	if err != nil || !reflect.DeepEqual(got, frame) || wsproto.HeaderValue(got, "k") != "v" {
		t.Fatalf("decoded %#v, %v", got, err)
	}
}

func TestMalformedFrames(t *testing.T) {
	for _, wire := range []string{"", "00", "0880", "0800100018002200", "08001000180020002a020a00", "08001000180020004201"} {
		t.Run(wire, func(t *testing.T) {
			raw, err := hex.DecodeString(wire)
			if err != nil {
				t.Fatal(err)
			}
			var official larkws.Frame
			if _, err := wsproto.Decode(raw); err == nil || proto.Unmarshal(raw, &official) == nil {
				t.Fatal("malformed frame accepted")
			}
		})
	}
}

func TestControlDataAndResponse(t *testing.T) {
	config := larkws.ClientConfig{ReconnectCount: -1, PingInterval: 120}
	pong, err := wsproto.ControlFrame(7, wsproto.Pong, &config)
	if err != nil || pong.Service != 7 || pong.Method != wsproto.Control || wsproto.HeaderValue(pong, wsproto.Type) != "pong" {
		t.Fatalf("invalid pong: %+v, %v", pong, err)
	}
	var gotConfig larkws.ClientConfig
	if err := json.Unmarshal(pong.Payload, &gotConfig); err != nil || gotConfig != config {
		t.Fatalf("pong config = %+v, %v", gotConfig, err)
	}
	for _, kind := range []string{wsproto.Event, wsproto.Card} {
		f := wsproto.DataFrame(7, kind, "message", "trace", []byte("payload"))
		f.Headers = append(f.Headers, wsproto.Header{Key: wsproto.BizRT, Value: "3"})
		h := larkws.Headers(f.Headers)
		if f.Method != 1 || h.GetString(larkws.HeaderType) != kind || h.GetString(larkws.HeaderMessageID) != "message" ||
			h.GetString(larkws.HeaderTraceID) != "trace" || h.GetInt(larkws.HeaderSum) != 1 ||
			h.GetString(larkws.HeaderSeq) != "0" || h.GetInt(larkws.HeaderBizRt) != 3 {
			t.Fatalf("invalid data headers: %+v", h)
		}
	}
	response, err := wsproto.DecodeResponse([]byte(`{"code":200,"headers":{"x-key":"value"},"data":"aGVsbG8="}`))
	if err != nil || response.StatusCode != 200 || response.Headers["x-key"] != "value" || string(response.Data) != "hello" {
		t.Fatalf("invalid response: %+v, %v", response, err)
	}
	for _, invalid := range []string{`{"code":200,"data":"!"}`, `{"headers":{}}`, `null`, `{`} {
		if _, err := wsproto.DecodeResponse([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid response %s", invalid)
		}
	}
}
