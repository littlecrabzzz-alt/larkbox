package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/cardkit"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

const cardActionTrigger = "card.action.trigger"

// CardActionTriggerInput describes a simulated click or form submission.
type CardActionTriggerInput struct {
	MessageID      string
	OperatorOpenID string
	Action         callback.CallBackAction
	Host           string
	DeliveryType   string
	URL            string
	PreviewToken   string
}

// CardActionCard is the card update returned by the callback receiver.
type CardActionCard struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// CardActionResult captures the callback response and its persistence outcome.
type CardActionResult struct {
	EventID  string
	Delivery Delivery
	Toast    *callback.Toast
	Card     *CardActionCard
	Updated  bool
}

type cardOperator struct {
	callback.Operator
	UnionID string `json:"union_id"`
}

type cardActionPayload struct {
	Operator     cardOperator            `json:"operator"`
	Token        string                  `json:"token"`
	Action       callback.CallBackAction `json:"action"`
	Host         string                  `json:"host"`
	DeliveryType string                  `json:"delivery_type,omitempty"`
	Context      callback.Context        `json:"context"`
}

// TriggerCardAction delivers one synchronous callback to one receiver. A live
// long connection takes precedence over the webhook. Delivery is never retried.
func (d *Dispatcher) TriggerCardAction(ctx context.Context, input CardActionTriggerInput) (CardActionResult, error) {
	ctx, finish, err := d.begin(ctx)
	if err != nil {
		return CardActionResult{}, err
	}
	defer finish()
	target, err := d.state.CardActionTarget(input.MessageID, input.OperatorOpenID)
	if err != nil {
		return CardActionResult{}, err
	}
	message, user := target.Message, target.Operator
	app, ok := d.state.Apps().Get(message.AppID)
	if !ok {
		return CardActionResult{}, errors.New("interactive message has no known app")
	}
	host := input.Host
	if host == "" {
		host = "im_message"
	}
	if host != "im_message" && host != "im_top_notice" {
		return CardActionResult{}, errors.New("invalid card action host")
	}
	if input.DeliveryType != "" && input.DeliveryType != "url_preview" {
		return CardActionResult{}, errors.New("invalid card action delivery type")
	}
	tenant := user.TenantKey
	if tenant == "" {
		tenant = tenantKey(app)
	}
	payload := cardActionPayload{
		Operator: cardOperator{
			Operator: callback.Operator{TenantKey: &tenant, UserID: &user.UserID, OpenID: user.OpenID},
			UnionID:  user.UnionID,
		},
		Token: protocol.NewID(""), Action: input.Action, Host: host, DeliveryType: input.DeliveryType,
		Context: callback.Context{
			URL: input.URL, PreviewToken: input.PreviewToken, OpenMessageID: message.ID, OpenChatID: message.ChatID,
		},
	}
	job, err := d.newJob(ctx, message.AppID, cardActionTrigger, d.state.Now(), payload)
	if err != nil {
		return CardActionResult{}, err
	}
	result := CardActionResult{EventID: job.id}
	verify := func(ctx context.Context, body []byte) error {
		body = bytes.TrimSpace(body)
		if len(body) == 0 || bytes.Equal(body, []byte("null")) {
			return nil
		}
		var reply struct {
			Toast *callback.Toast `json:"toast"`
			Card  *CardActionCard `json:"card"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
			return fmt.Errorf("invalid card action response: %w", err)
		}
		result.Toast, result.Card = reply.Toast, reply.Card
		if reply.Card == nil {
			return nil
		}
		card, reference, err := cardkit.ParseCallbackCard(d.state, reply.Card.Type, reply.Card.Data)
		if err != nil {
			return fmt.Errorf("invalid card action response card: %w", err)
		}
		if err := d.state.ApplyCardAction(ctx, target, card, reference); err != nil {
			return fmt.Errorf("apply card action response: %w", err)
		}
		result.Updated = true
		return nil
	}

	result.Delivery = d.deliverCardAction(ctx, job, verify)
	return result, result.Delivery.Err
}

func (d *Dispatcher) deliverCardAction(ctx context.Context, job deliveryJob, verify func(context.Context, []byte) error) Delivery {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if session := d.selectSession(ctx, job.app.AppID, true); session != nil {
		return d.withDeliveryFault(ctx, job, "websocket", "", socketPath, func(job deliveryJob) Delivery {
			return d.socketAttempt(ctx, session, job, 1, firstCallback(job.copy, verify))
		})
	}
	if job.app.WebhookURL != "" {
		if err := job.prepareWebhook(); err != nil {
			return Delivery{Channel: "webhook", Err: err}
		}
		return d.withDeliveryFault(ctx, job, "webhook", http.MethodPost, job.webhookPath(), func(job deliveryJob) Delivery {
			return d.webhookAttempt(ctx, job, 1, d.opts.WebhookTimeout, firstCallback(job.copy, verify))
		})
	}
	err := ctx.Err()
	if err == nil {
		err = errors.New("app has no active card action receiver")
	}
	d.record(job.epoch, job.trace("dispatcher", 0), err)
	return Delivery{Channel: "dispatcher", Err: err}
}

func firstCallback(index int, verify func(context.Context, []byte) error) func(context.Context, []byte) error {
	if index == 0 {
		return verify
	}
	return nil
}
