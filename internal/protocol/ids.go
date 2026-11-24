package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"strconv"
)

// IDPrefix identifies the prefix of a generated Feishu or sandbox ID.
type IDPrefix string

// Prefixes used for Feishu entities, credentials, and sandbox events.
const (
	OpenID       IDPrefix = "ou_"      // User or bot open ID.
	UnionID      IDPrefix = "on_"      // User union ID.
	ChatID       IDPrefix = "oc_"      // Chat ID.
	MessageID    IDPrefix = "om_"      // IM message ID.
	ImageKey     IDPrefix = "img_v3_"  // Image key.
	FileKey      IDPrefix = "file_v3_" // File key.
	TenantToken  IDPrefix = "t-"       // Tenant access token.
	AppToken     IDPrefix = "a-"       // App access token.
	UserToken    IDPrefix = "u-"       // User access token.
	RefreshToken IDPrefix = "ur-"      // OAuth refresh token.
	OAuthCode    IDPrefix = ""         // OAuth authorization code.
	CardID       IDPrefix = "card_"    // Selects a decimal CardKit ID.
	EventID      IDPrefix = "evt_"     // Event ID.
	FrameID      IDPrefix = "msg_"     // Message frame ID.
)

// NewID appends 128 random bits in hexadecimal to prefix. CardID instead
// selects a 19-digit decimal ID that fits the CardKit wire format.
func NewID(prefix IDPrefix) string {
	if prefix == CardID {
		const floor = 1_000_000_000_000_000_000
		value, err := rand.Int(rand.Reader, big.NewInt(8*floor))
		if err != nil {
			panic(err) // The Go 1.24 default crypto/rand reader cannot return an error.
		}
		return strconv.FormatInt(floor+value.Int64(), 10)
	}
	var random [16]byte
	// Go 1.24 guarantees that rand.Read fills the buffer or terminates the process.
	_, _ = rand.Read(random[:])
	return string(prefix) + hex.EncodeToString(random[:])
}
