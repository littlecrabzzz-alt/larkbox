package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// VerifyWebhook sends the Feishu URL verification challenge to an app's
// configured receiver and checks that it echoes the challenge.
func (d *Dispatcher) VerifyWebhook(ctx context.Context, appID string) error {
	app, ok := d.state.Apps().Get(appID)
	if !ok || app.WebhookURL == "" {
		return errors.New("app has no configured webhook")
	}
	ctx, finish, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()

	challenge := protocol.NewID("")
	body, err := json.Marshal(map[string]string{
		"challenge": challenge, "token": app.VerificationToken, "type": "url_verification",
	})
	if err != nil {
		return err
	}
	job := deliveryJob{app: app, kind: "url_verification", body: body}
	delivery := d.webhookAttempt(ctx, job, 1, d.opts.VerificationTimeout, func(body []byte) error {
		var echo struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(body, &echo); err != nil {
			return fmt.Errorf("invalid webhook challenge response: %w", err)
		}
		if echo.Challenge != challenge {
			return errors.New("webhook challenge mismatch")
		}
		return nil
	})
	return delivery.Err
}

func (d *Dispatcher) deliverWebhook(ctx context.Context, job deliveryJob) Delivery {
	var result Delivery
	for i := 1; i <= d.opts.WebhookMaxAttempts; i++ {
		if i > 1 && !d.canRetry(ctx) {
			break
		}
		result = d.webhookAttempt(ctx, job, i, d.opts.WebhookTimeout, nil)
		if result.Err == nil || i == d.opts.WebhookMaxAttempts || !d.waitRetry(ctx, i, nil) {
			break
		}
	}
	return result
}

func (d *Dispatcher) webhookAttempt(ctx context.Context, job deliveryJob, attempt int, timeout time.Duration, verify func([]byte) error) Delivery {
	trace := job.trace("webhook", attempt)
	trace.Method = http.MethodPost
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := d.postWebhook(ctx, job.app, &trace)
	if err == nil && verify != nil {
		err = verify(trace.ResponseBody)
		if err == nil {
			err = ctx.Err()
		}
	}
	d.record(trace, err)
	return Delivery{Channel: trace.Channel, Status: trace.Status, Headers: trace.ResponseHeaders, Data: trace.ResponseBody, Err: err}
}

func (d *Dispatcher) postWebhook(ctx context.Context, app store.App, trace *store.Trace) error {
	target, err := url.Parse(app.WebhookURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return errors.New("invalid webhook URL")
	}
	trace.Path = target.Path
	if trace.Path == "" {
		trace.Path = "/"
	}
	body := trace.RequestBody
	if app.EncryptKey != "" {
		body, err = protocol.EncryptWebhook(app.EncryptKey, body)
		if err != nil {
			return err
		}
	}
	trace.RequestBody = body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if app.EncryptKey != "" {
		timestamp, nonce := strconv.FormatInt(d.state.Now().Unix(), 10), protocol.NewID("")
		req.Header.Set("X-Lark-Request-Timestamp", timestamp)
		req.Header.Set("X-Lark-Request-Nonce", nonce)
		req.Header.Set("X-Lark-Signature", protocol.WebhookSignature(timestamp, nonce, app.EncryptKey, body))
	}
	trace.RequestHeaders = req.Header
	// #nosec G704 -- Deliver to the webhook URL configured by the sandbox operator.
	resp, err := d.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("post webhook: %w", urlErr.Err)
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	trace.Status, trace.ResponseHeaders = resp.StatusCode, resp.Header
	trace.ResponseBody, err = io.ReadAll(io.LimitReader(resp.Body, maxFrame+1))
	if err != nil {
		return err
	}
	if len(trace.ResponseBody) > maxFrame {
		return errors.New("webhook response exceeds size limit")
	}
	if !success(trace.Status) {
		return fmt.Errorf("webhook HTTP status %d", trace.Status)
	}
	return nil
}
