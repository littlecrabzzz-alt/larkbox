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
	return d.VerifyWebhookAt(ctx, appID, "").Err
}

// VerifyWebhookAt verifies the configured URL or a one-time override.
func (d *Dispatcher) VerifyWebhookAt(ctx context.Context, appID, override string) Delivery {
	ctx, finish, err := d.begin(ctx)
	if err != nil {
		return Delivery{Err: err}
	}
	defer finish()
	epoch := ctx.Value(admissionKey{}).(admission).epoch
	app, ok := d.state.Apps().Get(appID)
	if ok && override != "" {
		app.WebhookURL = override
	}
	if !ok || app.WebhookURL == "" {
		return Delivery{Err: errors.New("app has no configured webhook")}
	}
	challenge := protocol.NewID("")
	body, _ := json.Marshal(map[string]string{
		"challenge": challenge, "token": app.VerificationToken, "type": "url_verification",
	})
	job := deliveryJob{app: app, kind: "url_verification", requestID: store.RequestID(ctx), body: body, epoch: epoch, redactor: store.AppRedactor(app)}
	if err := job.prepareWebhook(); err != nil {
		return Delivery{Err: err}
	}
	return d.webhookAttempt(ctx, job, 1, d.opts.VerificationTimeout, func(_ context.Context, body []byte) error {
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
}

func (job *deliveryJob) prepareWebhook() error {
	job.wireBody = job.body
	if job.app.EncryptKey == "" {
		return nil
	}
	var err error
	job.wireBody, err = protocol.EncryptWebhook(job.app.EncryptKey, job.body)
	return err
}

func (job deliveryJob) webhookPath() string {
	target, err := url.Parse(job.app.WebhookURL)
	if err == nil && target.EscapedPath() != "" {
		return target.EscapedPath()
	}
	return "/"
}

func (d *Dispatcher) deliverWebhook(ctx context.Context, job deliveryJob) Delivery {
	if err := job.prepareWebhook(); err != nil {
		return Delivery{Channel: "webhook", Err: err}
	}
	return d.withDeliveryFault(ctx, job, "webhook", http.MethodPost, job.webhookPath(), func(job deliveryJob) Delivery {
		return d.webhookDelivery(ctx, job)
	})
}

func (d *Dispatcher) webhookDelivery(ctx context.Context, job deliveryJob) Delivery {
	return d.webhookDeliveryFrom(ctx, job, 1)
}

func (d *Dispatcher) webhookDeliveryFrom(ctx context.Context, job deliveryJob, start int) (result Delivery) {
	defer func() { d.finishRetry(ctx, job, "webhook", result.Err) }()
	for i := start; i <= max(start, d.opts.WebhookMaxAttempts); i++ {
		if i > 1 && !d.canRetry(ctx) {
			if result.Err == nil {
				result.Err = context.Canceled
			}
			break
		}
		result = d.webhookAttempt(ctx, job, i, d.opts.WebhookTimeout, nil)
		if result.Err == nil || i >= d.opts.WebhookMaxAttempts || !d.waitRetry(ctx, job, "webhook", i, result.completedAt, nil) {
			break
		}
	}
	return result
}

func (d *Dispatcher) webhookAttempt(ctx context.Context, job deliveryJob, attempt int, timeout time.Duration, verify func(context.Context, []byte) error) Delivery {
	trace := job.trace("webhook", attempt)
	trace.Method = http.MethodPost
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := d.postWebhook(ctx, job.app, job.wireBody, &trace)
	if err == nil && job.kind == cardActionTrigger && trace.Status != http.StatusOK {
		err = fmt.Errorf("webhook HTTP status %d", trace.Status)
	}
	if err == nil {
		err = ctx.Err()
		if err == nil && verify != nil {
			err = verify(ctx, trace.ResponseBody)
		}
	}
	completedAt := d.state.Now()
	d.record(job, trace, err)
	return Delivery{Channel: trace.Channel, Status: trace.Status, Headers: trace.ResponseHeaders, Data: trace.ResponseBody, Err: err, completedAt: completedAt}
}

func (d *Dispatcher) postWebhook(ctx context.Context, app store.App, body []byte, trace *store.Trace) error {
	target, err := url.Parse(app.WebhookURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return errors.New("invalid webhook URL")
	}
	trace.Path = target.EscapedPath()
	if trace.Path == "" {
		trace.Path = "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if app.EncryptKey != "" {
		timestamp, nonce := strconv.FormatInt(time.Now().Unix(), 10), protocol.NewID("")
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
		trace.ResponseBodyTruncated = true
		return errors.New("webhook response exceeds size limit")
	}
	if !success(trace.Status) {
		return fmt.Errorf("webhook HTTP status %d", trace.Status)
	}
	return nil
}
