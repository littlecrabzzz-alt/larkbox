package work

import (
	"net/http"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

func (h handler) minute(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.state.Minute(h.owner(r), r.PathValue("minute_token"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"minute": fields{"token": entry.Token, "title": entry.Title,
		"duration": strconv.FormatInt(entry.DurationMS, 10), "owner_id": entry.OwnerOpenID}})
}

func (h handler) minuteArtifacts(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.state.Minute(h.owner(r), r.PathValue("minute_token"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"transcript": entry.Transcript, "summary": entry.Summary})
}
