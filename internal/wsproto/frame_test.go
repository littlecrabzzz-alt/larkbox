package wsproto_test

import (
	"bytes"
	"encoding/binary"
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

func TestVarintOverflowBoundaries(t *testing.T) {
	const (
		fields   = "0800100018002000"
		overlong = "808080808080808080"
		maxValue = "ffffffffffffffffff01"
		overflow = "ffffffffffffffffff02"
	)
	for _, tc := range []struct {
		name, wire string
		valid      bool
	}{
		{"max uint64", "08" + maxValue + "100018002000", true},
		{"maximum tenth byte is one", "08" + overlong + "01" + "100018002000", true},
		{"overflowing frame value", "08" + overflow + "100018002000", false},
		{"overflowing frame tag", fields + "d0808080808080808002" + "00", false},
		{"overflowing frame payload length", fields + "42" + overlong + "02", false},
		{"valid ten-byte header length", fields + "2a0d0a" + overlong + "00" + "1200", true},
		{"overflowing header length", fields + "2a0d0a" + overlong + "02" + "1200", false},
		{"overflowing varint in unknown header field", fields + "2a0f0a00120018" + overflow, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := hex.DecodeString(tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := wsproto.Decode(wire)
			if (err == nil) != tc.valid {
				t.Fatalf("Decode returned %+v, %v", frame, err)
			}
			if tc.name == "max uint64" && frame.SeqID != math.MaxUint64 {
				t.Fatalf("decoded %d, want %d", frame.SeqID, uint64(math.MaxUint64))
			}
		})
	}
}

func TestFieldNumberBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field uint64
		valid bool
	}{
		{"zero", 0, false},
		{"maximum", 1<<29 - 1, true},
		{"above maximum", 1 << 29, false},
		{"aliases field one after int32 conversion", 1<<32 | 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := []byte{0x08, 0, 0x10, 0, 0x18, 0, 0x20, 0}
			tag := tc.field << 3
			outer := binary.AppendUvarint(bytes.Clone(base), tag)
			outer = append(outer, 0)
			header := binary.AppendUvarint([]byte{0x0a, 0, 0x12, 0}, tag)
			header = append(header, 0)
			nested := append(bytes.Clone(base), 0x2a)
			nested = binary.AppendUvarint(nested, uint64(len(header))) // #nosec G115 -- slice length is nonnegative.
			nested = append(nested, header...)
			for _, wire := range [][]byte{outer, nested} {
				if _, err := wsproto.Decode(wire); (err == nil) != tc.valid {
					t.Errorf("Decode(%x) returned %v, valid = %t", wire, err, tc.valid)
				}
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
