package store

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// MailDraft is a saved composition in the local mailbox.
type MailDraft struct {
	Owner                  ResourceOwner
	ID, From, Subject, Raw string
	Text, HTML, SentID     string
	To, CC, BCC            []string
}

// MailMessage is one mailbox copy of a simulated delivery.
type MailMessage struct {
	MailDraft
	MessageID, ThreadID, Folder string
	SentAt                      time.Time
}

// Mailbox resolves the caller's own email address or its me alias.
func (s *Store) Mailbox(owner ResourceOwner, selector string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, hasApp := s.apps.items[owner.AppID]
	user, hasUser := s.users.items[owner.UserOpenID]
	if !hasApp || !hasUser || user.Inactive || user.TenantKey != app.TenantKey || (user.Email == "" && user.EnterpriseEmail == "") {
		return User{}, false
	}
	if !strings.EqualFold(selector, "me") && !userMailAddress(user, selector) {
		return User{}, false
	}
	return cloneUser(user), true
}

func userMailAddress(user User, address string) bool {
	return address != "" && (strings.EqualFold(address, user.Email) || strings.EqualFold(address, user.EnterpriseEmail))
}

// SaveMailDraft saves a composition with its parsed body and envelope.
func (s *Store) SaveMailDraft(draft MailDraft) MailDraft {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft.ID = protocol.NewID("md_")
	s.work.drafts[draft.ID] = cloneMailDraft(draft)
	return draft
}

// MailDraft reads one draft visible to its creating mailbox.
func (s *Store) MailDraft(owner ResourceOwner, id string) (MailDraft, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	draft, ok := s.work.drafts[id]
	if !ok || draft.Owner != owner || draft.SentID != "" {
		return MailDraft{}, false
	}
	return cloneMailDraft(draft), true
}

// SendMailDraft delivers a saved draft within the sandbox. Repeating a send
// returns the original sent message without adding another mailbox copy.
func (s *Store) SendMailDraft(owner ResourceOwner, id string) (MailMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.work.drafts[id]
	if !ok || draft.Owner != owner {
		return MailMessage{}, ErrNotFound
	}
	if draft.SentID != "" {
		return cloneMail(s.work.mail[draft.SentID]), nil
	}
	recipients := slices.Concat(draft.To, draft.CC, draft.BCC)
	if len(recipients) == 0 {
		return MailMessage{}, ErrNotFound
	}
	app, hasApp := s.apps.items[owner.AppID]
	sender, hasUser := s.users.items[owner.UserOpenID]
	if !hasApp || !hasUser || sender.Inactive || sender.TenantKey != app.TenantKey || !userMailAddress(sender, draft.From) {
		return MailMessage{}, ErrTokenOwner
	}
	message := MailMessage{
		MailDraft: cloneMailDraft(draft), MessageID: protocol.NewID("mm_"), ThreadID: protocol.NewID("mt_"), Folder: "SENT", SentAt: s.Now(),
	}
	s.work.mail[message.MessageID] = message
	draft.SentID = message.MessageID
	s.work.drafts[draft.ID] = draft
	for _, user := range s.users.items {
		if user.Inactive || user.TenantKey != sender.TenantKey || !slices.ContainsFunc(recipients, func(address string) bool { return userMailAddress(user, address) }) {
			continue
		}
		incoming := cloneMail(message)
		incoming.Owner = ResourceOwner{AppID: owner.AppID, UserOpenID: user.OpenID}
		incoming.MessageID, incoming.Folder = protocol.NewID("mm_"), "INBOX"
		incoming.BCC = nil
		incoming.Raw = ""
		s.work.mail[incoming.MessageID] = incoming
	}
	return cloneMail(message), nil
}

// MailboxMessages returns copies owned by one mailbox, newest first.
func (s *Store) MailboxMessages(owner ResourceOwner) []MailMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]MailMessage, 0)
	for _, message := range s.work.mail {
		if message.Owner == owner {
			result = append(result, cloneMail(message))
		}
	}
	slices.SortFunc(result, func(a, b MailMessage) int {
		if order := b.SentAt.Compare(a.SentAt); order != 0 {
			return order
		}
		return cmp.Compare(a.MessageID, b.MessageID)
	})
	return result
}

func cloneMailDraft(draft MailDraft) MailDraft {
	draft.To, draft.CC, draft.BCC = slices.Clone(draft.To), slices.Clone(draft.CC), slices.Clone(draft.BCC)
	return draft
}

func cloneMail(message MailMessage) MailMessage {
	message.MailDraft = cloneMailDraft(message.MailDraft)
	return message
}
