//go:build live

package livetest

import (
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
	golden(t, "cardkit/card_create_ok", created.ApiResp)
	id := value(created.Data.CardId)

	stream := func(text string) *larkcore.ApiResp {
		t.Helper()
		resp, err := api.CardElement.Content(t.Context(), larkcardkit.NewContentCardElementReqBuilder().CardId(id).ElementId("md").
			Body(larkcardkit.NewContentCardElementReqBodyBuilder().Sequence(1).Content(text).Build()).Build(), tenant)
		requireSDK(t, "stream card element", err)
		return resp.ApiResp
	}
	expectOK(t, stream("updated"))
	golden(t, "cardkit/card_element_content_stale_seq_300317", stream("duplicate sequence"))

	closed, err := api.Card.Settings(t.Context(), larkcardkit.NewSettingsCardReqBuilder().CardId(id).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().Sequence(2).Settings(`{"config":{"streaming_mode":false}}`).Build()).Build(), tenant)
	requireSDK(t, "close streaming mode", err)
	expectOK(t, closed.ApiResp)
}
