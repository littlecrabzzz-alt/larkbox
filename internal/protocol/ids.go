package protocol

import (
	"crypto/rand"
	"encoding/hex"
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
	RefreshToken IDPrefix = "r-"       // OAuth refresh token.
	CardID       IDPrefix = "card_"    // CardKit card ID.
	EventID      IDPrefix = "evt_"     // Event ID.
	FrameID      IDPrefix = "msg_"     // Message frame ID.
)

// NewID appends 128 random bits, encoded in hexadecimal, to prefix.
func NewID(prefix IDPrefix) string {
	var random [16]byte
	// Go 1.24 guarantees that rand.Read fills the buffer or terminates the process.
	_, _ = rand.Read(random[:])
	return string(prefix) + hex.EncodeToString(random[:])
}
