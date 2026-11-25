package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

// Snapshot is a decoded store image. It is consumed by RestoreSnapshot.
type Snapshot struct{ image storeImage }

type storeImage struct {
	Apps                map[string]App
	Users               map[string]User
	UserOrder           []User
	Departments         map[string]config.Department
	ApprovalDefinitions map[string]config.ApprovalDefinition
	ApprovalInstances   map[string]ApprovalInstance
	ApprovalSubscribers map[string]map[string]bool
	ApprovalReplays     []approvalReplayImage
	Chats               map[string]chatImage
	DisbandedChats      map[string]struct{}
	Messages            map[string]messageImage
	MessageReplays      []messageReplayImage
	Media               map[string]Media
	Cards               map[string]Card
	CardReplays         []cardReplayImage
	CardTemplates       []config.CardTemplate
	LatestTemplates     map[string]string
	MessageOrder        uint64
	Revision            uint64
	Tokens              map[string]Token
	Authorizations      []Authorization
	DeviceFlows         map[string]DeviceFlow
	DeviceUserCodes     map[string]string
	Documents           map[string]Document
	WikiSpaces          map[string]WikiSpace
	WikiNodes           map[string]WikiNode
	DriveFiles          map[string]DriveFile
	Spreadsheets        map[string]sheetImage
	Bases               map[string]baseImage
	Calendar            map[string]CalendarEvent
	Tasks               map[string]Task
	Minutes             map[string]config.Minute
	Drafts              map[string]MailDraft
	Mail                map[string]MailMessage
}

type chatImage struct {
	Chat
	AppID, CreatorID string
	BotManagers      []string
	CreatedAt        time.Time
	MemberOrder      map[string]uint64
	NextMemberOrder  uint64
}

type messageImage struct {
	Message
	ForwardedIDs   []string
	ForwardedCount int
	InvalidSources []string
}

type approvalReplayImage struct {
	AppID, TenantKey, UUID, Code string
	Input                        ApprovalStart
}

type messageReplayImage struct {
	AppID, Operation, UUID, ForwardTarget string
	ID, Target                            string
	Actor                                 IMActor
	Expires                               time.Time
}

type cardReplayImage struct {
	AppID, CardID, Operation, UUID string
	Fingerprint                    [32]byte
	Expires                        time.Time
}

type sheetImage struct {
	Spreadsheet
	Cells []cellImage
}

type cellImage struct {
	Position [2]int
	Value    json.RawMessage
}

type baseImage struct {
	Base
	Records []BaseRecord
}

// ExportSnapshot serializes the live state while holding a consistent read view.
func (s *Store) ExportSnapshot() (json.RawMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	image := storeImage{
		Apps: s.apps.items, Users: s.users.items, UserOrder: s.userSeedOrder,
		Departments: s.departments.items, ApprovalDefinitions: s.approvalDefinitions.items,
		ApprovalInstances: s.approvalInstances, ApprovalSubscribers: s.approvalSubscribers,
		Chats: make(map[string]chatImage, len(s.chats.items)), DisbandedChats: s.disbandedChats,
		Messages: make(map[string]messageImage, len(s.messages.items)), Media: s.media.items, Cards: s.cards.items,
		LatestTemplates: s.latestTemplates, MessageOrder: s.messageOrder, Revision: s.revision,
		Tokens: s.tokens, Authorizations: slices.Collect(maps.Values(s.authorizations)),
		DeviceFlows: s.deviceFlows, DeviceUserCodes: s.deviceUserCodes,
		Documents: s.content.docs, WikiSpaces: s.content.spaces, WikiNodes: s.content.nodes, DriveFiles: s.content.files,
		Spreadsheets: make(map[string]sheetImage, len(s.content.sheets)), Bases: make(map[string]baseImage, len(s.content.bases)),
		Calendar: s.work.events, Tasks: s.work.tasks, Minutes: s.work.minutes, Drafts: s.work.drafts, Mail: s.work.mail,
	}
	for id, chat := range s.chats.items {
		image.Chats[id] = chatImage{chat, chat.AppID, chat.CreatorID, chat.BotManagers, chat.CreatedAt, chat.MemberOrder, chat.NextMemberOrder}
	}
	for id, message := range s.messages.items {
		image.Messages[id] = messageImage{message, message.forwardedIDs, message.forwardedCount, message.invalidSources}
	}
	for k, replay := range s.approvalRequests {
		image.ApprovalReplays = append(image.ApprovalReplays, approvalReplayImage{k.appID, k.tenantKey, k.uuid, replay.code, replay.input})
	}
	for k, replay := range s.messageUUIDs {
		image.MessageReplays = append(image.MessageReplays, messageReplayImage{k.appID, k.operation, k.uuid, k.forwardTarget,
			replay.id, replay.target, replay.actor, replay.expires})
	}
	for k, replay := range s.cardUUIDs {
		image.CardReplays = append(image.CardReplays, cardReplayImage{k.appID, k.cardID, k.operation, k.uuid, replay.fingerprint, replay.expires})
	}
	for key, data := range s.cardTemplates {
		image.CardTemplates = append(image.CardTemplates, config.CardTemplate{TemplateID: key.id, VersionName: key.version, Data: data})
	}
	for id, spreadsheet := range s.content.sheets {
		cells := make([]cellImage, 0, len(spreadsheet.cells))
		for pos, value := range spreadsheet.cells {
			cells = append(cells, cellImage{pos, value})
		}
		image.Spreadsheets[id] = sheetImage{spreadsheet, cells}
	}
	for id, base := range s.content.bases {
		image.Bases[id] = baseImage{base, base.records}
	}
	return json.Marshal(image)
}

// DecodeSnapshot validates a store image before any runtime state is replaced.
func DecodeSnapshot(raw json.RawMessage) (*Snapshot, error) {
	var image storeImage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&image) != nil || !errors.Is(dec.Decode(new(json.RawMessage)), io.EOF) ||
		image.Apps == nil || image.Users == nil || image.Chats == nil || image.Messages == nil || image.Tokens == nil ||
		!validKeys(image.Apps, func(app App) string { return app.AppID }) ||
		!validKeys(image.Users, func(user User) string { return user.OpenID }) ||
		!validKeys(image.Chats, func(chat chatImage) string { return chat.ChatID }) ||
		!validKeys(image.Messages, func(message messageImage) string { return message.ID }) ||
		!validKeys(image.Cards, func(card Card) string { return card.ID }) ||
		!validKeys(image.Media, func(media Media) string { return media.Key }) ||
		!validKeys(image.Tokens, func(token Token) string { return token.Value }) {
		return nil, errors.New("invalid snapshot store image")
	}
	for _, app := range image.Apps {
		if app.AppSecret == "" || app.BotOpenID == "" {
			return nil, errors.New("invalid app in snapshot")
		}
		if app.WebhookURL != "" {
			if _, err := config.ParseRedirectURI(app.WebhookURL); err != nil {
				return nil, errors.New("invalid webhook in snapshot")
			}
		}
	}
	return &Snapshot{image}, nil
}

func validKeys[T any](entries map[string]T, key func(T) string) bool {
	for id, entry := range entries {
		if id == "" || id != key(entry) {
			return false
		}
	}
	return true
}

func replaceMap[K comparable, V any](target *map[K]V, image map[K]V) {
	if image != nil {
		*target = image
	}
}

// RestoreSnapshot restores the live state while retaining the configured reset baseline.
func (s *Store) RestoreSnapshot(snapshot *Snapshot) {
	s.resetGate.Lock()
	defer s.resetGate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	image := snapshot.image
	s.reset(config.Config{})
	s.revision, s.messageOrder = image.Revision, image.MessageOrder
	s.userSeedOrder = image.UserOrder
	replaceMap(&s.apps.items, image.Apps)
	replaceMap(&s.users.items, image.Users)
	replaceMap(&s.departments.items, image.Departments)
	replaceMap(&s.approvalDefinitions.items, image.ApprovalDefinitions)
	replaceMap(&s.approvalInstances, image.ApprovalInstances)
	replaceMap(&s.approvalSubscribers, image.ApprovalSubscribers)
	replaceMap(&s.disbandedChats, image.DisbandedChats)
	replaceMap(&s.media.items, image.Media)
	replaceMap(&s.cards.items, image.Cards)
	replaceMap(&s.tokens, image.Tokens)
	replaceMap(&s.deviceFlows, image.DeviceFlows)
	replaceMap(&s.deviceUserCodes, image.DeviceUserCodes)
	replaceMap(&s.content.docs, image.Documents)
	replaceMap(&s.content.spaces, image.WikiSpaces)
	replaceMap(&s.content.nodes, image.WikiNodes)
	replaceMap(&s.content.files, image.DriveFiles)
	replaceMap(&s.work.events, image.Calendar)
	replaceMap(&s.work.tasks, image.Tasks)
	replaceMap(&s.work.minutes, image.Minutes)
	replaceMap(&s.work.drafts, image.Drafts)
	replaceMap(&s.work.mail, image.Mail)
	for id, entry := range image.Chats {
		chat := entry.Chat
		chat.AppID, chat.CreatorID, chat.BotManagers = entry.AppID, entry.CreatorID, entry.BotManagers
		chat.CreatedAt, chat.MemberOrder, chat.NextMemberOrder = entry.CreatedAt, entry.MemberOrder, entry.NextMemberOrder
		s.chats.items[id] = chat
	}
	for id, entry := range image.Messages {
		message := entry.Message
		message.forwardedIDs, message.forwardedCount, message.invalidSources = entry.ForwardedIDs, entry.ForwardedCount, entry.InvalidSources
		message.DiagnosticRedactor = AppRedactor(s.apps.items[message.AppID])
		s.messages.items[id] = message
		s.indexMessage(Message{}, message)
	}
	for id, card := range s.cards.items {
		card.DiagnosticRedactor = AppRedactor(s.apps.items[card.AppID])
		if bytes.Equal(card.TemplateData, []byte("null")) {
			card.TemplateData = nil
		}
		if bytes.Equal(card.JSON, []byte("null")) {
			card.JSON = nil
		}
		s.cards.items[id] = card
	}
	for _, replay := range image.ApprovalReplays {
		s.approvalRequests[approvalRequest{replay.AppID, replay.TenantKey, replay.UUID}] = approvalReplay{input: replay.Input, code: replay.Code}
	}
	for _, replay := range image.MessageReplays {
		s.messageUUIDs[messageRequest{replay.AppID, replay.Operation, replay.UUID, replay.ForwardTarget}] =
			messageDedup{replay.ID, replay.Target, replay.Actor, replay.Expires}
	}
	for _, replay := range image.CardReplays {
		s.cardUUIDs[cardRequest{replay.AppID, replay.CardID, replay.Operation, replay.UUID}] = cardReplay{replay.Fingerprint, replay.Expires}
	}
	for _, grant := range image.Authorizations {
		s.authorizations[authorizationKey{grant.AppID, grant.OpenID}] = grant
	}
	for _, template := range image.CardTemplates {
		s.cardTemplates[templateKey{template.TemplateID, template.VersionName}] = template.Data
	}
	replaceMap(&s.latestTemplates, image.LatestTemplates)
	for id, entry := range image.Spreadsheets {
		sheet := entry.Spreadsheet
		sheet.cells = make(map[[2]int]json.RawMessage, len(entry.Cells))
		for _, cell := range entry.Cells {
			sheet.cells[cell.Position] = cell.Value
		}
		s.content.sheets[id] = sheet
	}
	for id, entry := range image.Bases {
		base := entry.Base
		base.records = entry.Records
		s.content.bases[id] = base
	}
	snapshot.image = storeImage{}
}
