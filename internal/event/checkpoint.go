package event

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// PendingRetry represents the next at-least-once delivery of an event.
type PendingRetry struct {
	AppID, TenantKey, EventID, EventType, RequestID, Channel string
	Copy, Attempt                                            int
	Due                                                      time.Time
	Body                                                     json.RawMessage
}

type retryKey struct {
	id, channel string
	copy        int
}

func (p PendingRetry) key() retryKey { return retryKey{p.EventID, p.Channel, p.Copy} }

// ValidatePending checks the shape and uniqueness of persisted retries.
func ValidatePending(retries []PendingRetry) error {
	seen := make(map[retryKey]bool, len(retries))
	for _, retry := range retries {
		key := retry.key()
		if retry.AppID == "" || retry.EventID == "" || retry.EventType == "" || retry.Due.IsZero() ||
			(retry.Channel != "webhook" && retry.Channel != "websocket") ||
			retry.Copy < 0 || retry.Attempt < 2 || len(retry.Body) > maxFrame || !json.Valid(retry.Body) || seen[key] {
			return errors.New("invalid pending event retry")
		}
		seen[key] = true
	}
	return nil
}

// ExportCheckpoint captures the store and admitted retries in one generation.
func (d *Dispatcher) ExportCheckpoint(capture func([]PendingRetry) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	retries := make([]PendingRetry, 0, len(d.retryJournal))
	for _, retry := range d.retryJournal {
		retries = append(retries, retry)
	}
	return capture(retries)
}

func (d *Dispatcher) scheduleRetry(ctx context.Context, job deliveryJob, channel string, failed int, due time.Time) {
	entry := PendingRetry{AppID: job.app.AppID, TenantKey: job.app.TenantKey, EventID: job.id, EventType: job.kind,
		RequestID: job.requestID, Channel: channel, Copy: job.copy, Attempt: failed + 1, Due: due, Body: job.body}
	d.mu.Lock()
	defer d.mu.Unlock()
	if scope, ok := ctx.Value(admissionKey{}).(admission); ok && scope.reset == d.resetCtx {
		d.retryJournal[entry.key()] = entry
	}
}

func (d *Dispatcher) finishRetry(ctx context.Context, job deliveryJob, channel string, err error) {
	if err != nil {
		select {
		case <-d.retriesStopped:
			// Keep the last eligible attempt for a graceful checkpoint.
			return
		default:
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if scope, ok := ctx.Value(admissionKey{}).(admission); ok && scope.reset == d.resetCtx {
		delete(d.retryJournal, retryKey{job.id, channel, job.copy})
	}
}

// RestoreCheckpoint replaces runtime state and starts the restored retry waits.
func (d *Dispatcher) RestoreCheckpoint(restore func(), retries []PendingRetry) {
	d.Reset(func() {
		restore()
		for _, retry := range retries {
			d.resumeRetry(retry)
		}
	})
}

// resumeRetry requires the dispatcher lock and the restored store.
func (d *Dispatcher) resumeRetry(retry PendingRetry) {
	app, ok := d.state.Apps().Get(retry.AppID)
	if !ok || app.TenantKey != retry.TenantKey || d.closed ||
		(retry.Channel == "webhook" && app.WebhookURL == "") {
		return
	}
	epoch := d.state.TraceEpoch()
	job := deliveryJob{app: app, id: retry.EventID, kind: retry.EventType, requestID: retry.RequestID,
		body: retry.Body, epoch: epoch, copy: retry.Copy, redactor: store.AppRedactor(app)}
	ctx := context.WithValue(d.resetCtx, admissionKey{}, admission{d.resetCtx, epoch})
	d.retryJournal[retry.key()] = retry
	d.pending++
	d.active.Add(1)
	go d.runRestoredRetry(ctx, job, retry)
}

func (d *Dispatcher) runRestoredRetry(ctx context.Context, job deliveryJob, retry PendingRetry) {
	defer d.active.Done()
	defer func() {
		d.mu.Lock()
		d.pending--
		d.mu.Unlock()
	}()
	if !d.waitUntilRetry(ctx, retry.Due, nil) {
		d.finishRetry(ctx, job, retry.Channel, context.Canceled)
		return
	}
	if retry.Channel == "webhook" {
		if err := job.prepareWebhook(); err != nil {
			d.finishRetry(ctx, job, retry.Channel, err)
			d.record(job, job.trace(retry.Channel, retry.Attempt), err)
			return
		}
		d.webhookDeliveryFrom(ctx, job, retry.Attempt)
		return
	}
	if peer := d.waitForRestoredPeer(ctx, job.app.AppID); peer != nil {
		d.socketDeliveryFrom(ctx, peer, job, retry.Attempt)
		return
	}
	err := errors.New("no reconnected websocket receiver")
	d.finishRetry(ctx, job, retry.Channel, err)
	d.record(job, job.trace(retry.Channel, retry.Attempt), err)
}

func (d *Dispatcher) waitForRestoredPeer(ctx context.Context, appID string) *session {
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	for d.canRetry(ctx) {
		d.mu.Lock()
		changed := d.socketChanged
		d.mu.Unlock()
		if peer := d.selectSession(ctx, appID, false); peer != nil {
			return peer
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-d.retriesStopped:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}
