package sandbox

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func (c *control) traces(w http.ResponseWriter, r *http.Request) {
	const maxLimit = 256
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid trace query")
		return
	}
	for key, values := range query {
		switch key {
		case "direction", "app_id", "method", "path_prefix", "event_type", "event_id", "channel", "limit":
			if len(values) == 1 {
				continue
			}
		}
		fail(w, http.StatusBadRequest, "unknown or repeated trace filter")
		return
	}
	filter := store.TraceFilter{
		AppID: query.Get("app_id"), Method: strings.ToUpper(query.Get("method")), PathPrefix: query.Get("path_prefix"),
		EventType: query.Get("event_type"), EventID: query.Get("event_id"), Channel: query.Get("channel"), Limit: maxLimit,
	}
	switch query.Get("direction") {
	case "", "all":
	case "inbound", "outbound":
		filter.Direction = store.TraceDirection(query.Get("direction"))
	default:
		fail(w, http.StatusBadRequest, "direction must be all, inbound, or outbound")
		return
	}
	if filter.Channel != "" && filter.Channel != "webhook" && filter.Channel != "websocket" {
		fail(w, http.StatusBadRequest, "channel must be webhook or websocket")
		return
	}
	if query.Has("limit") {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > maxLimit {
			fail(w, http.StatusBadRequest, "limit must be between 1 and 256")
			return
		}
		filter.Limit = limit
	}
	traces := c.state.QueryTraces(filter)
	result := make([]data, 0, len(traces))
	for _, trace := range traces {
		result = append(result, data{
			"direction": trace.Direction, "started_at": trace.StartedAt, "latency_ms": trace.Latency.Milliseconds(),
			"app_id": trace.AppID, "method": trace.Method, "path": trace.Path, "status_code": trace.Status, "code": trace.Code,
			"request_headers": trace.RequestHeaders, "request_body": traceBody(trace.RequestBody),
			"request_body_truncated": trace.RequestBodyTruncated, "response_headers": trace.ResponseHeaders,
			"response_body": traceBody(trace.ResponseBody), "response_body_truncated": trace.ResponseBodyTruncated,
			"event_id": trace.EventID, "event_type": trace.EventType, "channel": trace.Channel,
			"attempt": trace.Attempt, "duplicate_index": trace.Copy, "error": trace.Error,
		})
	}
	reply(w, http.StatusOK, data{"traces": result})
}

func traceBody(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	return json.RawMessage(body)
}

func (c *control) clearTraces(w http.ResponseWriter, _ *http.Request) {
	c.state.ClearTraces()
	reply(w, http.StatusOK, data{"cleared": true})
}

func (c *control) listFaults(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, data{"faults": c.faults.List()})
}

func (c *control) addFault(w http.ResponseWriter, r *http.Request) {
	var spec fault.Spec
	if !read(w, r, &spec, false) {
		return
	}
	c.mu.RLock()
	rule, err := c.faults.Add(spec)
	c.mu.RUnlock()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Location", "/_sandbox/faults/"+rule.ID)
	reply(w, http.StatusCreated, rule)
}

func (c *control) deleteFault(w http.ResponseWriter, r *http.Request) {
	if !c.faults.Delete(r.PathValue("fault_id")) {
		fail(w, http.StatusNotFound, "fault rule not found")
		return
	}
	reply(w, http.StatusOK, data{"deleted": true})
}

func (c *control) clearFaults(w http.ResponseWriter, _ *http.Request) {
	c.faults.Reset()
	reply(w, http.StatusOK, data{"cleared": true})
}
