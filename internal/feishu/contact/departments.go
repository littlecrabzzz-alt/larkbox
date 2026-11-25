package contact

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func departmentData(view store.Directory, entry config.Department, ids idTypes) fields {
	var leader string
	if user, ok := view.UserByID("open_id", entry.LeaderOpenID); ok {
		leader = user.ContactID(ids.user)
	}
	members := 0
	for _, user := range view.Users {
		if !user.Inactive && slices.Contains(user.DepartmentOpenIDs, entry.OpenID) {
			members++
		}
	}
	return fields{
		"name": entry.Name, "open_department_id": entry.OpenID, "department_id": entry.ID,
		"parent_department_id": view.DepartmentID(entry.ParentOpenID, ids.department),
		"leader_user_id":       leader, "member_count": members, "status": fields{"is_deleted": false},
	}
}

func (h handler) getDepartment(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	dept, ok := visibleDepartment(view, ids.department, r.PathValue("department_id"))
	if !ok {
		notFound(w, 41017, "department not exist")
		return
	}
	reply(w, fields{"department": departmentData(view, dept, ids)})
}

func (h handler) listDepartments(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	parent, ok := visibleDepartment(view, ids.department, cmp.Or(r.PathValue("department_id"), query.Get("parent_department_id"), "0"))
	if !ok {
		notFound(w, 41017, "department not exist")
		return
	}
	recursive := false
	if query.Has("fetch_child") {
		var err error
		recursive, err = strconv.ParseBool(query.Get("fetch_child"))
		if err != nil {
			invalid(w)
			return
		}
	}
	entries := make([]fields, 0)
	for _, dept := range view.Departments {
		if cmp.Or(dept.ParentOpenID, "0") == parent.OpenID || (recursive && departmentDescendant(view, dept, parent.OpenID)) {
			entries = append(entries, departmentData(view, dept, ids))
		}
	}
	listPage(w, r, entries, 50)
}

func departmentDescendant(view store.Directory, child config.Department, ancestor string) bool {
	if ancestor == "0" {
		return true
	}
	for hops := 0; hops < len(view.Departments); hops++ {
		parent := child.ParentOpenID
		if parent == ancestor {
			return true
		}
		var ok bool
		child, ok = visibleDepartment(view, "open_department_id", parent)
		if !ok || child.OpenID == "0" {
			return false
		}
	}
	return false
}

func (h handler) scopes(w http.ResponseWriter, r *http.Request) {
	view, ok := h.directory(w, r)
	if !ok {
		return
	}
	ids, ok := types(w, r)
	if !ok {
		return
	}
	type scopeEntry struct {
		id         string
		department bool
	}
	var items []scopeEntry
	if scope := view.ContactScope; scope == nil {
		items = append(items, scopeEntry{"0", true})
	} else {
		for _, id := range scope.DepartmentOpenIDs {
			items = append(items, scopeEntry{view.DepartmentID(id, ids.department), true})
		}
		for _, id := range scope.UserOpenIDs {
			if user, ok := view.UserByID("open_id", id); ok && user.ContactID(ids.user) != "" {
				items = append(items, scopeEntry{user.ContactID(ids.user), false})
			}
		}
	}
	page, next, ok := paginate(w, r, items, 100)
	if !ok {
		return
	}
	departments, users := make([]string, 0), make([]string, 0)
	for _, entry := range page {
		if entry.department {
			departments = append(departments, entry.id)
		} else {
			users = append(users, entry.id)
		}
	}
	reply(w, fields{"department_ids": departments, "user_ids": users, "group_ids": []string{}, "has_more": next != "", "page_token": next})
}
