// Package approval implements native approval instances and task decisions.
package approval

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type fields = map[string]any

type handler struct {
	state  *store.Store
	events *event.Dispatcher
}

// RegisterRoutes serves instance lifecycle and approval definition subscriptions.
func RegisterRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store, events *event.Dispatcher) {
	h := handler{state, events}
	tenant := middleware.Bearer(state, middleware.Tenant, cfg.StrictMode)
	mux.Handle("POST /open-apis/approval/v4/instances", tenant(http.HandlerFunc(h.create)))
	mux.Handle("GET /open-apis/approval/v4/instances/{instance_id}", tenant(http.HandlerFunc(h.read)))
	mux.Handle("POST /open-apis/approval/v4/instances/cancel", tenant(h.decide(store.ApprovalCanceled)))
	mux.Handle("POST /open-apis/approval/v4/tasks/approve", tenant(h.decide(store.ApprovalApproved)))
	mux.Handle("POST /open-apis/approval/v4/tasks/reject", tenant(h.decide(store.ApprovalRejected)))
	mux.Handle("POST /open-apis/approval/v4/approvals/{approval_code}/subscribe", tenant(h.subscribe(true)))
	mux.Handle("POST /open-apis/approval/v4/approvals/{approval_code}/unsubscribe", tenant(h.subscribe(false)))
}

func (h handler) appID(r *http.Request) string {
	if identity, ok := middleware.AuthFromContext(r.Context()); ok {
		return identity.AppID
	}
	if apps := h.state.Apps().List(); len(apps) > 0 {
		return apps[0].AppID
	}
	return ""
}

func (h handler) subscribe(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appID := h.appID(r)
		err := h.state.Mutate(r.Context(), func() error {
			return h.state.SubscribeApproval(appID, r.PathValue("approval_code"), enabled)
		})
		if err != nil {
			failure(w, err)
			return
		}
		reply(w, nil)
	}
}

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Definition string `json:"approval_code"`
		UserID     string `json:"user_id"`
		OpenID     string `json:"open_id"`
		Department string `json:"department_id"`
		Form       string `json:"form"`
		UUID       string `json:"uuid"`
	}
	if !decode(w, r, &input) {
		return
	}
	form := bytes.TrimSpace([]byte(input.Form))
	if input.Definition == "" || (input.OpenID == "" && input.UserID == "") || len(form) == 0 || form[0] != '[' || !json.Valid(form) {
		failure(w, protocol.NewError(protocol.InvalidParameter, nil))
		return
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, form)
	appID := h.appID(r)
	change, err := store.Commit(r.Context(), h.state, func() (store.ApprovalChange, error) {
		return h.state.StartApproval(appID, store.ApprovalStart{
			DefinitionCode: input.Definition, UserID: input.UserID, OpenID: input.OpenID, DepartmentID: input.Department,
			Form: compact.String(), UUID: input.UUID,
		})
	})
	if err != nil {
		failure(w, err)
		return
	}
	h.notify(r, change)
	reply(w, fields{"instance_code": change.Instance.Code})
}

func (h handler) read(w http.ResponseWriter, r *http.Request) {
	instance, err := h.state.ReadApproval(h.appID(r), r.PathValue("instance_id"))
	if err != nil {
		failure(w, err)
		return
	}
	reply(w, instanceData(instance))
}

func (h handler) decide(status store.ApprovalStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Definition string `json:"approval_code"`
			Instance   string `json:"instance_code"`
			UserID     string `json:"user_id"`
			TaskID     string `json:"task_id"`
			Comment    string `json:"comment"`
		}
		if !decode(w, r, &input) {
			return
		}
		idType := r.URL.Query().Get("user_id_type")
		if idType == "" {
			idType = "open_id"
		}
		if !slices.Contains([]string{"open_id", "user_id", "union_id"}, idType) || input.Definition == "" || input.Instance == "" ||
			input.UserID == "" || (status != store.ApprovalCanceled && input.TaskID == "") {
			failure(w, protocol.NewError(protocol.InvalidParameter, nil))
			return
		}
		appID := h.appID(r)
		change, err := store.Commit(r.Context(), h.state, func() (store.ApprovalChange, error) {
			return h.state.DecideApproval(appID, store.ApprovalDecision{
				DefinitionCode: input.Definition, InstanceCode: input.Instance, ActorID: input.UserID, IDType: idType,
				TaskID: input.TaskID, Comment: input.Comment, Status: status,
			})
		})
		if err != nil {
			failure(w, err)
			return
		}
		h.notify(r, change)
		reply(w, nil)
	}
}

func (h handler) notify(r *http.Request, change store.ApprovalChange) {
	instance := change.Instance
	at := instance.CompletedAt
	if at.IsZero() {
		at = instance.StartedAt
	}
	payload := fields{
		"approval_code": instance.DefinitionCode, "instance_code": instance.Code, "status": instance.Status,
		"operate_time": millis(at), "instance_operate_time": millis(at), "uuid": protocol.NewID(""),
	}
	for _, appID := range change.Subscribers {
		h.events.Enqueue(r.Context(), appID, event.ApprovalInstance, at, payload)
	}
}

func instanceData(instance store.ApprovalInstance) fields {
	actor := instance.Approver
	taskStatus := string(instance.Status)
	decision := taskStatus
	switch instance.Status {
	case store.ApprovalApproved:
		decision = "PASS"
	case store.ApprovalRejected:
		decision = "REJECT"
	case store.ApprovalCanceled:
		actor, taskStatus, decision = instance.Submitter, "DONE", "CANCEL"
	}
	timeline := []fields{{"type": "START", "user_id": instance.Submitter.UserID, "open_id": instance.Submitter.OpenID, "create_time": millis(instance.StartedAt)}}
	if !instance.CompletedAt.IsZero() {
		timeline = append(timeline, fields{"type": decision, "user_id": actor.UserID, "open_id": actor.OpenID,
			"create_time": millis(instance.CompletedAt), "task_id": instance.TaskID, "comment": instance.Comment})
	}
	task := fields{
		"id": instance.TaskID, "user_id": instance.Approver.UserID, "open_id": instance.Approver.OpenID,
		"status": taskStatus, "node_id": "review", "node_key": "review", "type": "AND",
		"start_time": millis(instance.StartedAt), "end_time": millis(instance.CompletedAt),
	}
	return fields{
		"approval_name": instance.DefinitionName, "approval_code": instance.DefinitionCode, "instance_code": instance.Code,
		"status": instance.Status, "open_id": instance.Submitter.OpenID, "user_id": instance.Submitter.UserID,
		"form": instance.Form, "uuid": instance.UUID, "department_id": instance.DepartmentID,
		"start_time": millis(instance.StartedAt), "end_time": millis(instance.CompletedAt),
		"task_list": []fields{task}, "timeline": timeline, "comment_list": []fields{},
	}
}

func millis(at time.Time) string {
	if at.IsZero() {
		return "0"
	}
	return strconv.FormatInt(at.UnixMilli(), 10)
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || !errors.Is(decoder.Decode(new(json.RawMessage)), io.EOF) {
		failure(w, protocol.NewError(protocol.MalformedRequest, nil))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func failure(w http.ResponseWriter, err error) {
	if middleware.WriteResetConflict(w, err) {
		return
	}
	var apiErr protocol.APIError
	if !errors.As(err, &apiErr) {
		code := protocol.InvalidParameter
		if errors.Is(err, store.ErrNotFound) {
			code = 60003
		}
		apiErr = protocol.APIError{Code: code, Message: err.Error(), Status: http.StatusBadRequest}
	}
	_ = protocol.WriteError(w, apiErr)
}
