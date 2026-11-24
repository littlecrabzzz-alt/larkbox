// Package store holds concurrency-safe virtual Feishu state for one server.
package store

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

// App is a seeded virtual application and its bot.
type App = config.App

// User is a virtual Feishu user.
type User = config.User

// Chat is a virtual p2p or group conversation.
type Chat = config.Chat

// TokenKind identifies the type of a stored credential.
type TokenKind string

// Credential types stored by the sandbox.
const (
	TenantToken  TokenKind = "tenant_access_token" // Tenant API access credential.
	AppToken     TokenKind = "app_access_token"    // App API access credential.
	UserToken    TokenKind = "user_access_token"   // User API access credential.
	RefreshToken TokenKind = "refresh_token"       // OAuth refresh credential.
	OAuthCode    TokenKind = "code"                // OAuth authorization code.
)

// Token stores a credential, its ownership, and its validity window.
type Token struct {
	Value      string
	Kind       TokenKind
	AppID      string
	UserOpenID string
	ExpiresAt  time.Time
	Revoked    bool
}

// Mention describes a user or bot referenced by a message.
type Mention struct {
	Key    string
	OpenID string
	UserID string
	Name   string
}

// Reaction records an emoji reaction and its operator.
type Reaction struct {
	ID             string
	OperatorOpenID string
	EmojiType      string
	CreatedAt      time.Time
}

// Message stores an IM message and its reply, recall, and reaction state.
type Message struct {
	ID           string
	AppID        string
	ChatID       string
	SenderOpenID string
	RootID       string
	ParentID     string
	ThreadID     string
	MsgType      string
	Content      string
	Mentions     []Mention
	Deleted      bool
	Updated      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Reactions    []Reaction
}

// Card stores a CardKit entity and its last accepted sequence.
type Card struct {
	ID            string
	AppID         string
	JSON          json.RawMessage
	Elements      map[string]string
	StreamingMode bool
	Sequence      int64
}

// Media stores an image or file and its download metadata.
type Media struct {
	Key         string
	AppID       string
	Bytes       []byte
	ContentType string
	Filename    string
}

// TraceDirection identifies how an audited exchange originated.
type TraceDirection string

// Directions recorded in the trace ring.
const (
	Inbound  TraceDirection = "inbound"  // Open API request received by larkbox.
	Outbound TraceDirection = "outbound" // Event delivery sent by larkbox.
)

// Trace captures an API exchange or a single outbound delivery attempt.
type Trace struct {
	Direction             TraceDirection
	StartedAt             time.Time
	Method                string
	Path                  string
	RequestHeaders        http.Header
	RequestBody           []byte
	RequestBodyTruncated  bool
	ResponseHeaders       http.Header
	ResponseBody          []byte
	ResponseBodyTruncated bool
	Status                int
	Code                  *int
	Latency               time.Duration
	AppID                 string
	EventID               string
	Channel               string
	Attempt               int
}
