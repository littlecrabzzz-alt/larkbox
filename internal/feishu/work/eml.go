package work

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func parseDraft(raw string, owner store.ResourceOwner, mailbox store.User) (store.MailDraft, error) {
	encoded := strings.TrimRight(raw, "=")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return store.MailDraft{}, err
	}
	message, err := mail.ReadMessage(bytes.NewReader(decoded))
	if err != nil {
		return store.MailDraft{}, err
	}
	from, err := message.Header.AddressList("From")
	if err != nil || len(from) != 1 || !strings.EqualFold(from[0].Address, mailbox.Email) && !strings.EqualFold(from[0].Address, mailbox.EnterpriseEmail) {
		return store.MailDraft{}, errors.New("sender must belong to the mailbox")
	}
	var draft store.MailDraft
	draft.Owner, draft.Raw, draft.From = owner, raw, from[0].Address
	draft.Subject, err = new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		return store.MailDraft{}, err
	}
	for _, field := range []struct {
		header string
		into   *[]string
	}{{"To", &draft.To}, {"Cc", &draft.CC}, {"Bcc", &draft.BCC}} {
		if message.Header.Get(field.header) == "" {
			continue
		}
		list, err := message.Header.AddressList(field.header)
		if err != nil {
			return store.MailDraft{}, err
		}
		for _, address := range list {
			*field.into = append(*field.into, address.Address)
		}
	}
	err = parseBody(textproto.MIMEHeader(message.Header), message.Body, &draft, 0)
	return draft, err
}

func parseBody(header textproto.MIMEHeader, body io.Reader, draft *store.MailDraft, depth int) error {
	if depth > 8 {
		return errors.New("invalid MIME nesting")
	}
	kind, params, err := mime.ParseMediaType(cmp.Or(header.Get("Content-Type"), "text/plain"))
	if err != nil {
		return err
	}
	disposition, _, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	if disposition == "attachment" {
		return nil
	}
	switch strings.ToLower(header.Get("Content-Transfer-Encoding")) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	if strings.HasPrefix(kind, "multipart/") {
		parts := multipart.NewReader(body, params["boundary"])
		for {
			part, err := parts.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := parseBody(part.Header, part, draft, depth+1); err != nil {
				return err
			}
		}
	}
	switch kind {
	case "text/plain", "text/html":
		content, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		if kind == "text/plain" {
			draft.Text = string(content)
		} else {
			draft.HTML = string(content)
		}
	}
	return nil
}
