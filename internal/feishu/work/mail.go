package work

import (
	"cmp"
	"encoding/base64"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	mailReadScope    = "mail:user_mailbox.message:readonly"
	mailAddressScope = "mail:user_mailbox.message.address:read"
	mailSubjectScope = "mail:user_mailbox.message.subject:read"
	mailBodyScope    = "mail:user_mailbox.message.body:read"
)

func (h handler) mailbox(w http.ResponseWriter, r *http.Request) (store.ResourceOwner, store.User, bool) {
	owner := h.owner(r)
	user, ok := h.state.Mailbox(owner, r.PathValue("mailbox_id"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
	}
	return owner, user, ok
}

func (h handler) mailProfile(w http.ResponseWriter, r *http.Request) {
	if _, user, ok := h.mailbox(w, r); ok {
		reply(w, fields{"primary_email_address": cmp.Or(user.EnterpriseEmail, user.Email)})
	}
}

func (h handler) createMailDraft(w http.ResponseWriter, r *http.Request) {
	owner, user, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	var input struct {
		Raw string `json:"raw"`
	}
	if !read(w, r, &input, false) {
		return
	}
	draft, err := parseDraft(input.Raw, owner, user)
	if err != nil {
		fail(w, protocol.InvalidParameter)
		return
	}
	saved, err := store.Commit(r.Context(), h.state, func() (store.MailDraft, error) {
		return h.state.SaveMailDraft(draft), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"draft_id": saved.ID})
}

func (h handler) readMailDraft(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	draft, found := h.state.MailDraft(owner, r.PathValue("draft_id"))
	if !found {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"draft_id": draft.ID, "raw": draft.Raw})
}

func (h handler) sendMailDraft(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	var input struct {
		SendTime string `json:"send_time"`
	}
	if !read(w, r, &input, true) {
		return
	}
	if input.SendTime != "" {
		fail(w, protocol.InvalidParameter)
		return
	}
	sent, err := store.Commit(r.Context(), h.state, func() (store.MailMessage, error) {
		return h.state.SendMailDraft(owner, r.PathValue("draft_id"))
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"message_id": sent.MessageID, "thread_id": sent.ThreadID})
}

func addressData(addresses []string) []fields {
	result := make([]fields, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, fields{"mail_address": address})
	}
	return result
}

func mailScope(r *http.Request, scope string) bool {
	auth, signed := middleware.AuthFromContext(r.Context())
	return !signed || slices.Contains(strings.Fields(auth.Scope), scope)
}

func mailData(r *http.Request, msg store.MailMessage, format string) fields {
	item := fields{"message_id": msg.MessageID, "thread_id": msg.ThreadID, "folder_id": msg.Folder,
		"internal_date": milliseconds(msg.SentAt), "is_read": msg.Folder == "SENT", "label_ids": []string{}}
	if mailScope(r, mailAddressScope) {
		item["head_from"] = fields{"mail_address": msg.From}
		item["head_to"], item["head_cc"], item["head_bcc"] = addressData(msg.To), addressData(msg.CC), addressData(msg.BCC)
	}
	if mailScope(r, mailSubjectScope) {
		item["subject"] = msg.Subject
	}
	if format != "metadata" && mailScope(r, mailBodyScope) {
		item["body_plain_text"] = base64.RawURLEncoding.EncodeToString([]byte(msg.Text))
		if format == "full" {
			item["body_html"] = base64.RawURLEncoding.EncodeToString([]byte(msg.HTML))
		}
	}
	return item
}

func (h handler) readMail(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	for _, msg := range h.state.MailboxMessages(owner) {
		if msg.MessageID == r.PathValue("message_id") {
			reply(w, fields{"message": mailData(r, msg, r.URL.Query().Get("format"))})
			return
		}
	}
	fail(w, protocol.ResourceNotFound)
}

func (h handler) batchReadMail(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	var input struct {
		Format string   `json:"format"`
		IDs    []string `json:"message_ids"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if len(input.IDs) < 1 || len(input.IDs) > 20 {
		fail(w, protocol.InvalidParameter)
		return
	}
	messages := make([]fields, 0, len(input.IDs))
	for _, msg := range h.state.MailboxMessages(owner) {
		if slices.Contains(input.IDs, msg.MessageID) {
			messages = append(messages, mailData(r, msg, input.Format))
		}
	}
	reply(w, fields{"messages": messages})
}

func mailPage(w http.ResponseWriter, r *http.Request, maxPage int, items []fields) {
	query := r.URL.Query()
	offset, offsetErr := strconv.Atoi(cmp.Or(query.Get("page_token"), "0"))
	size, sizeErr := strconv.Atoi(cmp.Or(query.Get("page_size"), strconv.Itoa(maxPage)))
	if offsetErr != nil || sizeErr != nil || offset < 0 || size < 1 || size > maxPage {
		fail(w, protocol.InvalidParameter)
		return
	}
	start := min(offset, len(items))
	end := min(start+size, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	reply(w, fields{"items": items[start:end], "has_more": next != "", "page_token": next})
}

func (h handler) listMail(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	folder := cmp.Or(query.Get("folder_id"), "INBOX")
	if query.Get("label_id") != "" || (folder != "INBOX" && folder != "SENT") {
		fail(w, protocol.InvalidParameter)
		return
	}
	items := make([]fields, 0)
	for _, msg := range h.state.MailboxMessages(owner) {
		if msg.Folder == folder && (query.Get("only_unread") != "true" || msg.Folder == "INBOX") {
			items = append(items, fields{"message_id": msg.MessageID})
		}
	}
	mailPage(w, r, 20, items)
}

func (h handler) searchMail(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := h.mailbox(w, r)
	if !ok {
		return
	}
	var input struct {
		Query  string `json:"query"`
		Filter struct {
			Folder   []string `json:"folder"`
			Subject  string   `json:"subject"`
			IsUnread *bool    `json:"is_unread"`
		} `json:"filter"`
	}
	if !read(w, r, &input, false) {
		return
	}
	sought := strings.TrimSpace(input.Query)
	if len([]rune(sought)) > 50 {
		fail(w, protocol.InvalidParameter)
		return
	}
	items := make([]fields, 0)
	for _, msg := range h.state.MailboxMessages(owner) {
		if len(input.Filter.Folder) > 0 && !slices.ContainsFunc(input.Filter.Folder, func(folder string) bool { return strings.EqualFold(folder, msg.Folder) }) ||
			input.Filter.IsUnread != nil && *input.Filter.IsUnread != (msg.Folder == "INBOX") {
			continue
		}
		item := mailData(r, msg, "full")
		subject, _ := item["subject"].(string)
		if input.Filter.Subject != "" && !containsFold(subject, input.Filter.Subject) {
			continue
		}
		searchable := []string{subject}
		if mailScope(r, mailAddressScope) {
			searchable = append(searchable, msg.From)
			searchable = append(searchable, msg.To...)
			searchable = append(searchable, msg.CC...)
			searchable = append(searchable, msg.BCC...)
		}
		if mailScope(r, mailBodyScope) {
			searchable = append(searchable, msg.Text, msg.HTML)
		}
		if sought != "" && !slices.ContainsFunc(searchable, func(value string) bool { return containsFold(value, sought) }) {
			continue
		}
		items = append(items, fields{"meta_data": fields{"message_biz_id": msg.MessageID, "thread_id": msg.ThreadID,
			"title": subject, "from": item["head_from"], "create_time": msg.SentAt.Format("2006-01-02T15:04:05Z07:00"),
			"body_plain_text": item["body_plain_text"], "body_html": item["body_html"]}})
	}
	mailPage(w, r, 15, items)
}

func containsFold(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}
