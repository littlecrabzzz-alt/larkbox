package content

import (
	"cmp"
	"net/http"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func wikiSpaceData(space store.WikiSpace) fields {
	return fields{"space_id": space.ID, "name": space.Name, "description": space.Description, "space_type": "team", "visibility": "private", "open_sharing": "closed"}
}

func (h handler) createWikiSpace(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !read(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		fail(w, protocol.InvalidParameter)
		return
	}
	space, err := store.Commit(r.Context(), h.state, func() (store.WikiSpace, error) {
		return h.state.CreateWikiSpace(middleware.ResourceOwner(r), input.Name, input.Description), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"space": wikiSpaceData(space)})
}

func (h handler) readWikiSpace(w http.ResponseWriter, r *http.Request) {
	space, ok := h.state.WikiSpace(middleware.ResourceOwner(r), r.PathValue("space_id"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"space": wikiSpaceData(space)})
}

func (h handler) createWikiNode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ObjectType string `json:"obj_type"`
		Kind       string `json:"node_type"`
		Title      string `json:"title"`
		Parent     string `json:"parent_node_token"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.ObjectType != "docx" || input.Kind != "origin" {
		fail(w, protocol.InvalidParameter)
		return
	}
	node, err := store.Commit(r.Context(), h.state, func() (store.WikiNode, error) {
		return h.state.CreateWikiNode(middleware.ResourceOwner(r), r.PathValue("space_id"), input.Parent, cmp.Or(input.Title, "Untitled document"))
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"node": wikiNodeData(node)})
}

func (h handler) readWikiNode(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	key, kind := query.Get("token"), query.Get("obj_type")
	if kind == "" && strings.HasSuffix(r.URL.Path, "/get_node") {
		kind = "wiki"
	}
	if key == "" || (kind != "" && kind != "wiki" && kind != "docx") {
		fail(w, protocol.InvalidParameter)
		return
	}
	node, ok := h.state.WikiNode(middleware.ResourceOwner(r), key, kind)
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"node": wikiNodeData(node)})
}

func wikiNodeData(node store.WikiNode) fields {
	return fields{
		"space_id": node.SpaceID, "node_token": node.ID, "parent_node_token": node.ParentID,
		"obj_token": node.ObjectID, "obj_type": node.ObjectType, "node_type": node.Kind, "title": node.Title, "has_child": node.HasChild,
	}
}
