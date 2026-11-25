package contact

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func userStatus(user store.User) fields {
	return fields{"is_activated": !user.Inactive, "is_resigned": user.Inactive, "is_frozen": false, "is_exited": false, "is_unjoin": false}
}

func profile(view store.Directory, user store.User, ids idTypes) fields {
	departments := make([]string, 0, len(user.DepartmentOpenIDs))
	for _, id := range user.DepartmentOpenIDs {
		departments = append(departments, view.DepartmentID(id, ids.department))
	}
	return fields{
		"open_id": user.OpenID, "user_id": user.UserID, "union_id": user.UnionID,
		"name": user.Name, "en_name": user.EnName, "email": user.Email, "enterprise_email": user.EnterpriseEmail,
		"mobile": user.Mobile, "employee_no": user.EmployeeNo, "status": userStatus(user), "department_ids": departments,
		"avatar": fields{"avatar_origin": user.Avatar, "avatar_72": user.AvatarThumb, "avatar_240": user.AvatarMiddle, "avatar_640": user.AvatarBig},
	}
}

func (h handler) getUser(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	user, ok := view.UserByID(ids.user, r.PathValue("user_id"))
	if !ok {
		notFound(w, 41050, "user not found")
		return
	}
	reply(w, fields{"user": profile(view, user, ids)})
}

func (h handler) listUsers(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	parent := ""
	if id := r.URL.Query().Get("department_id"); id != "" {
		dept, ok := visibleDepartment(view, ids.department, id)
		if !ok {
			notFound(w, 41017, "department not exist")
			return
		}
		parent = dept.OpenID
	} else if strings.HasSuffix(r.URL.Path, "/find_by_department") {
		invalid(w)
		return
	}
	users := make([]fields, 0)
	for _, user := range view.Users {
		inDepartment := parent == "" || slices.Contains(user.DepartmentOpenIDs, parent) || (parent == "0" && len(user.DepartmentOpenIDs) == 0)
		if user.Inactive || !inDepartment {
			continue
		}
		users = append(users, profile(view, user, ids))
	}
	listPage(w, r, users, 50)
}

func (h handler) basicUsers(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	var input struct {
		IDs []string `json:"user_ids"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.IDs) == 0 || len(input.IDs) > 100 {
		invalid(w)
		return
	}
	users := make([]fields, 0)
	for _, id := range input.IDs {
		if user, found := view.UserByID(ids.user, id); found {
			users = append(users, fields{"user_id": user.ContactID(ids.user), "name": user.Name,
				"i18n_name": fields{"zh_cn": user.Name, "en_us": cmp.Or(user.EnName, user.Name)}})
		}
	}
	reply(w, fields{"users": users})
}

func (h handler) batchUserIDs(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	var input struct {
		Emails          []string `json:"emails"`
		Mobiles         []string `json:"mobiles"`
		IncludeResigned bool     `json:"include_resigned"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Emails)+len(input.Mobiles) == 0 || len(input.Emails) > 50 || len(input.Mobiles) > 50 {
		invalid(w)
		return
	}
	users := make([]fields, 0, len(input.Emails)+len(input.Mobiles))
	for _, selector := range []struct {
		kind   string
		values []string
	}{{"email", input.Emails}, {"mobile", input.Mobiles}} {
		for _, address := range selector.values {
			if strings.TrimSpace(address) == "" {
				invalid(w)
				return
			}
			item := fields{selector.kind: address}
			for _, user := range view.Users {
				match := selector.kind == "email" && strings.EqualFold(user.Email, address) || selector.kind == "mobile" && user.Mobile == address
				if match && (!user.Inactive || input.IncludeResigned) {
					item["user_id"], item["status"] = user.ContactID(ids.user), userStatus(user)
					break
				}
			}
			users = append(users, item)
		}
	}
	reply(w, fields{"user_list": users})
}

type searchFilter struct {
	UserIDs             []string `json:"user_ids"`
	IsResigned          bool     `json:"is_resigned"`
	HasContact          bool     `json:"has_contact"`
	ExcludeOuterContact bool     `json:"exclude_outer_contact"`
	HasEnterpriseEmail  bool     `json:"has_enterprise_email"`
}

func (h handler) searchUsers(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	var input struct {
		Query  string       `json:"query"`
		Filter searchFilter `json:"filter"`
	}
	if !decode(w, r, &input) {
		return
	}
	query := strings.ToLower(strings.TrimSpace(input.Query))
	filter := input.Filter
	if utf8.RuneCountInString(query) > 50 || len(filter.UserIDs) > 100 || (query == "" && len(filter.UserIDs) == 0 &&
		!filter.IsResigned && !filter.HasContact && !filter.ExcludeOuterContact && !filter.HasEnterpriseEmail) {
		invalid(w)
		return
	}
	items := make([]fields, 0)
	for _, user := range view.Users {
		if user.Inactive != filter.IsResigned || (len(filter.UserIDs) > 0 && !slices.Contains(filter.UserIDs, user.OpenID)) ||
			(filter.HasContact && view.DirectChats[user.OpenID] == "") || (filter.HasEnterpriseEmail && user.EnterpriseEmail == "") ||
			!matchesQuery(user, query) {
			continue
		}
		display := user.Name
		if len(user.DepartmentOpenIDs) > 0 {
			if dept, ok := visibleDepartment(view, "open_department_id", user.DepartmentOpenIDs[0]); ok {
				display += "\n" + dept.Name
			}
		}
		items = append(items, fields{"id": user.OpenID, "display_info": display, "meta_data": fields{
			"i18n_names":   fields{"zh_cn": user.Name, "en_us": cmp.Or(user.EnName, user.Name)},
			"mail_address": user.Email, "enterprise_mail_address": user.EnterpriseEmail, "is_registered": !user.Inactive,
			"chat_id": view.DirectChats[user.OpenID], "is_cross_tenant": false,
		}})
	}
	listPage(w, r, items, 30)
}

func matchesQuery(user store.User, query string) bool {
	return slices.ContainsFunc([]string{user.Name, user.EnName, user.Email, user.EnterpriseEmail, user.EmployeeNo,
		user.OpenID, user.UserID, user.UnionID}, func(value string) bool {
		return strings.Contains(strings.ToLower(value), query)
	})
}
