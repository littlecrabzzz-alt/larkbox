//go:build live

package livetest

import (
	"context"
	"net/http"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

func TestCardKitStreamingContract(t *testing.T) {
	h := newHarness(t)
	tenant := larkcore.WithTenantAccessToken(h.tenantToken(t))
	api := h.sdk.Cardkit.V1
	const card = `{"schema":"2.0","config":{"streaming_mode":true},"body":{"elements":[{"tag":"markdown","element_id":"md","content":"initial"}]}}`
	created, err := api.Card.Create(t.Context(), larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().Type("card_json").Data(card).Build()).Build(), tenant)
	requireSDK(t, "create streaming card", err)
	var closeStream func() *larkcore.ApiResp
	if created.Data != nil && value(created.Data.CardId) != "" {
		id := value(created.Data.CardId)
		closeAt := func(sequence int) mutation {
			return func(ctx context.Context) (*larkcore.ApiResp, error) {
				resp, err := api.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().CardId(id).
					Body(larkcardkit.NewSettingsCardReqBodyBuilder().Sequence(sequence).
						Settings(`{"config":{"streaming_mode":false}}`).Build()).Build(), tenant)
				if err != nil {
					return nil, err
				}
				return resp.ApiResp, nil
			}
		}
		closeStream = registerMutation(t, "close streaming mode", closeAt(2), closeAt(100))
	}
	h.checkResponse(t, "cardkit/card_create_ok", created.ApiResp, http.StatusOK,
		`{"code":0,"msg":"","data":{"card_id":"fixture:card_id"}}`)
	id := value(created.Data.CardId)

	stream := func(text string) *larkcore.ApiResp {
		t.Helper()
		resp, err := api.CardElement.Content(t.Context(), larkcardkit.NewContentCardElementReqBuilder().CardId(id).ElementId("md").
			Body(larkcardkit.NewContentCardElementReqBodyBuilder().Sequence(1).Content(text).Build()).Build(), tenant)
		requireSDK(t, "stream card element", err)
		return resp.ApiResp
	}
	h.expectOK(t, stream("updated"))
	h.checkResponse(t, "cardkit/card_element_content_stale_seq_300317", stream("duplicate sequence"),
		http.StatusBadRequest, liveErrorShape(300317))
	h.expectOK(t, closeStream())
}
