// Package contact implements the sandbox's Contact v3 reads.
package contact

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type fields = map[string]any

type handler struct {
	state  *store.Store
	strict bool
}

// RegisterRoutes serves the Contact v3 directory and recipient lookups.
func RegisterRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store) {
	h := handler{state: state, strict: cfg.StrictMode}
	register := func(pattern string, required middleware.RequiredToken, action http.HandlerFunc) {
		mux.Handle(pattern, middleware.Bearer(state, required, cfg.StrictMode)(action))
	}
	tenantOrUser := middleware.TenantOrUser
	register("POST /open-apis/contact/v3/users/search", middleware.User, h.searchUsers)
	register("POST /open-apis/contact/v3/users/batch_get_id", middleware.Tenant, h.batchUserIDs)
	register("POST /open-apis/contact/v3/users/basic_batch", tenantOrUser, h.basicUsers)
	register("GET /open-apis/contact/v3/users/{user_id}", tenantOrUser, h.getUser)
	register("GET /open-apis/contact/v3/users", tenantOrUser, h.listUsers)
	register("GET /open-apis/contact/v3/users/find_by_department", tenantOrUser, h.listUsers)
	register("GET /open-apis/contact/v3/departments/{department_id}", tenantOrUser, h.getDepartment)
	register("GET /open-apis/contact/v3/departments/{department_id}/children", tenantOrUser, h.listDepartments)
	register("GET /open-apis/contact/v3/departments", tenantOrUser, h.listDepartments)
	register("GET /open-apis/contact/v3/scopes", middleware.Tenant, h.scopes)
}

func (h handler) directory(w http.ResponseWriter, r *http.Request) (store.Directory, bool) {
	identity, ok := middleware.AuthFromContext(r.Context())
	if !ok {
		if apps := h.state.Apps().List(); len(apps) > 0 {
			identity.AppID = apps[0].AppID
		}
	}
	view, ok := h.state.Directory(identity.AppID, identity.UserOpenID)
	if !ok {
		_ = protocol.WriteError(w, protocol.NewError(protocol.InsufficientScope, nil))
	}
	return view, ok
}

func reply(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func invalid(w http.ResponseWriter) {
	_ = protocol.WriteError(w, protocol.NewError(protocol.InvalidParameter, nil))
}

func notFound(w http.ResponseWriter, code protocol.Code, message string) {
	_ = protocol.WriteError(w, protocol.APIError{Status: http.StatusBadRequest, Code: code, Message: message})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || !errors.Is(d.Decode(new(json.RawMessage)), io.EOF) {
		invalid(w)
		return false
	}
	return true
}

func choices(w http.ResponseWriter, r *http.Request, key string, options ...string) (string, bool) {
	values, given := r.URL.Query()[key]
	if !given {
		return options[0], true
	}
	if len(values) != 1 || !slices.Contains(options, values[0]) {
		invalid(w)
		return "", false
	}
	return values[0], true
}

type idTypes struct {
	user, department string
}

func types(w http.ResponseWriter, r *http.Request) (idTypes, bool) {
	user, ok := choices(w, r, "user_id_type", "open_id", "user_id", "union_id")
	if !ok {
		return idTypes{}, false
	}
	department, ok := choices(w, r, "department_id_type", "open_department_id", "department_id")
	return idTypes{user, department}, ok
}

func paginate[T any](w http.ResponseWriter, r *http.Request, items []T, maxSize int) ([]T, string, bool) {
	query := r.URL.Query()
	size := 20
	if requested := query.Get("page_size"); query.Has("page_size") {
		var err error
		size, err = strconv.Atoi(requested)
		if err != nil || size <= 0 || size > maxSize {
			invalid(w)
			return nil, "", false
		}
	}
	start := 0
	if cursor := query.Get("page_token"); cursor != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		n, parseErr := strconv.Atoi(string(raw))
		if decodeErr != nil || parseErr != nil || n <= 0 || n > len(items) {
			invalid(w)
			return nil, "", false
		}
		start = n
	}
	end := min(start+size, len(items))
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return items[start:end], next, true
}

func listPage[T any](w http.ResponseWriter, r *http.Request, items []T, maxSize int) {
	current, next, ok := paginate(w, r, items, maxSize)
	if ok {
		reply(w, fields{"items": current, "has_more": next != "", "page_token": next})
	}
}

func visibleDepartment(view store.Directory, kind, id string) (config.Department, bool) {
	if id == "0" {
		return config.Department{OpenID: "0", ID: "0", Name: "Root"}, true
	}
	for _, dept := range view.Departments {
		if view.DepartmentID(dept.OpenID, kind) == id {
			return dept, true
		}
	}
	return config.Department{}, false
}
