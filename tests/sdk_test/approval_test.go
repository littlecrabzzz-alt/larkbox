package sdk_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	larkapproval "github.com/larksuite/oapi-sdk-go/v3/service/approval/v4"
	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

func TestOfficialSDKApprovalLifecycle(t *testing.T) {
	type notification struct {
		Type, Token, UUID, TS string
		Event                 struct {
			AppID        string `json:"app_id"`
			Kind         string `json:"type"`
			Definition   string `json:"approval_code"`
			Instance     string `json:"instance_code"`
			Status       string `json:"status"`
			OperatedAt   string `json:"operate_time"`
			InstanceTime string `json:"instance_operate_time"`
		} `json:"event"`
	}
	inbox := make(chan notification, 12)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var incoming notification
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inbox <- incoming
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(webhook.Close)

	cfg := sdkTestConfig()
	alice, bob := cfg.Users[0], cfg.Users[1]
	definition := config.ApprovalDefinition{Code: "LARKBOX-EXPENSE", Name: "Expense review", ApproverOpenID: bob.OpenID}
	observer, outside := config.DefaultApp(), config.DefaultApp()
	observer.AppID, observer.BotOpenID = protocol.NewID("cli_"), "ou_approval_observer"
	observer.EventTypes, observer.EncryptKey, observer.WebhookURL = []string{event.ApprovalInstance}, "", webhook.URL
	outside.AppID, outside.BotOpenID, outside.TenantKey = protocol.NewID("cli_"), "ou_approval_outside", "outside"
	cfg.Apps = append(cfg.Apps, observer, outside)
	cfg.ApprovalDefinitions = []config.ApprovalDefinition{definition}
	srv, web := sdkEventServer(t, cfg, event.Options{})
	client, receiver := sdkEventREST(web, cfg.Apps[0]), sdkEventREST(web, observer)
	ctx := t.Context()
	registered, err := receiver.Approval.Approval.Subscribe(ctx, larkapproval.NewSubscribeApprovalReqBuilder().ApprovalCode(definition.Code).Build())
	requireSDKNetwork(t, err)
	if !registered.Success() {
		t.Fatalf("approval subscription failed: %s", registered.RawBody)
	}

	read := func(code string) *larkapproval.GetInstanceRespData {
		t.Helper()
		result, err := client.Approval.Instance.Get(ctx, larkapproval.NewGetInstanceReqBuilder().InstanceId(code).Build())
		requireSDKNetwork(t, err)
		if !result.Success() || result.Data == nil {
			t.Fatalf("cannot read approval instance: %s", result.RawBody)
		}
		return result.Data
	}
	observe := func(code, status string) *larkapproval.GetInstanceRespData {
		t.Helper()
		incoming := receiveSDKEvent(t, inbox)
		if incoming.Type != "event_callback" || incoming.Token != observer.VerificationToken || incoming.UUID == "" || incoming.TS == "" ||
			incoming.Event.AppID != observer.AppID || incoming.Event.Kind != event.ApprovalInstance || incoming.Event.Definition != definition.Code ||
			incoming.Event.Instance != code || incoming.Event.Status != status || incoming.Event.OperatedAt == "" || incoming.Event.InstanceTime != incoming.Event.OperatedAt {
			t.Fatalf("incorrect approval callback: %+v", incoming)
		}
		instance := read(code)
		if value(instance.Status) != status || value(instance.InstanceCode) != code {
			t.Fatalf("approval callback did not observe committed %s state", status)
		}
		return instance
	}

	const form = `[{"id":"expense","type":"input","value":"Travel"}]`
	create := func(uuid string) string {
		t.Helper()
		result, err := client.Approval.Instance.Create(ctx, larkapproval.NewCreateInstanceReqBuilder().
			InstanceCreate(larkapproval.NewInstanceCreateBuilder().ApprovalCode(definition.Code).OpenId(alice.OpenID).Form(form).Uuid(uuid).Build()).Build())
		requireSDKNetwork(t, err)
		if !result.Success() || result.Data == nil || value(result.Data.InstanceCode) == "" {
			t.Fatalf("cannot create an approval: %s", result.RawBody)
		}
		return value(result.Data.InstanceCode)
	}

	approved := create("approved-fixture")
	pending := observe(approved, "PENDING")
	if value(pending.Form) != form || value(pending.EndTime) != "0" || len(pending.TaskList) != 1 || value(pending.TaskList[0].OpenId) != bob.OpenID {
		t.Fatalf("incorrect pending approval: %+v", pending)
	}
	if duplicate := create("approved-fixture"); duplicate != approved {
		t.Fatal("repeated UUID created another approval instance")
	}
	forbidden, err := client.Approval.Task.Approve(ctx, larkapproval.NewApproveTaskReqBuilder().UserIdType("open_id").
		TaskApprove(larkapproval.NewTaskApproveBuilder().ApprovalCode(definition.Code).InstanceCode(approved).TaskId(value(pending.TaskList[0].Id)).UserId(alice.OpenID).Build()).Build())
	requireSDKNetwork(t, err)
	if forbidden.Success() || value(read(approved).Status) != "PENDING" {
		t.Fatal("an unrelated actor approved the task")
	}
	accepted, err := client.Approval.Task.Approve(ctx, larkapproval.NewApproveTaskReqBuilder().UserIdType("open_id").
		TaskApprove(larkapproval.NewTaskApproveBuilder().ApprovalCode(definition.Code).InstanceCode(approved).TaskId(value(pending.TaskList[0].Id)).UserId(bob.OpenID).Comment("Reviewed").Build()).Build())
	requireSDKNetwork(t, err)
	if !accepted.Success() {
		t.Fatalf("cannot approve: %s", accepted.RawBody)
	}
	done := observe(approved, "APPROVED")
	if value(done.EndTime) == "0" || len(done.Timeline) != 2 || value(done.Timeline[1].Comment) != "Reviewed" || value(done.TaskList[0].Status) != "APPROVED" {
		t.Fatalf("incorrect approval decision data: %+v", done)
	}

	rejected := create("rejected-fixture")
	pending = observe(rejected, "PENDING")
	declined, err := client.Approval.Task.Reject(ctx, larkapproval.NewRejectTaskReqBuilder().UserIdType("user_id").
		TaskApprove(larkapproval.NewTaskApproveBuilder().ApprovalCode(definition.Code).InstanceCode(rejected).TaskId(value(pending.TaskList[0].Id)).UserId(bob.UserID).Build()).Build())
	requireSDKNetwork(t, err)
	if !declined.Success() {
		t.Fatalf("cannot reject: %s", declined.RawBody)
	}
	observe(rejected, "REJECTED")

	canceled := create("canceled-fixture")
	observe(canceled, "PENDING")
	withdrawn, err := client.Approval.Instance.Cancel(ctx, larkapproval.NewCancelInstanceReqBuilder().UserIdType("open_id").
		InstanceCancel(larkapproval.NewInstanceCancelBuilder().ApprovalCode(definition.Code).InstanceCode(canceled).UserId(alice.OpenID).Build()).Build())
	requireSDKNetwork(t, err)
	if !withdrawn.Success() {
		t.Fatalf("cannot cancel: %s", withdrawn.RawBody)
	}
	observe(canceled, "CANCELED")

	isolated, err := sdkEventREST(web, outside).Approval.Instance.Get(ctx, larkapproval.NewGetInstanceReqBuilder().InstanceId(canceled).Build())
	requireSDKNetwork(t, err)
	if isolated.Success() || isolated.Code != 60003 {
		t.Fatalf("cross-tenant approval lookup returned %s", isolated.RawBody)
	}
	removed, err := receiver.Approval.Approval.Unsubscribe(ctx, larkapproval.NewUnsubscribeApprovalReqBuilder().ApprovalCode(definition.Code).Build())
	requireSDKNetwork(t, err)
	if !removed.Success() {
		t.Fatalf("cannot unsubscribe: %s", removed.RawBody)
	}
	create("unsubscribed-fixture")
	shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	requireSDKNetwork(t, srv.Shutdown(shutdown))
	if len(inbox) != 0 {
		t.Fatal("an idempotent retry, invalid action, or unsubscribed event was delivered")
	}
}
