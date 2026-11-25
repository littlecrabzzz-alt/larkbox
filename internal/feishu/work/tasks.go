package work

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func taskData(r *http.Request, task store.Task) fields {
	return fields{
		"guid": task.ID, "summary": task.Summary, "description": task.Description, "completed_at": milliseconds(task.Completed),
		"created_at": milliseconds(task.Created), "updated_at": milliseconds(task.Updated),
		"url": resourceURL(r, "/open-apis/task/v2/tasks/"+task.ID),
	}
}

func milliseconds(at time.Time) string {
	if at.IsZero() {
		return "0"
	}
	return strconv.FormatInt(at.UnixMilli(), 10)
}

func (h handler) createTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if strings.TrimSpace(input.Summary) == "" {
		fail(w, protocol.InvalidParameter)
		return
	}
	created, err := store.Commit(r.Context(), h.state, func() (store.Task, error) {
		return h.state.CreateTask(middleware.ResourceOwner(r), input.Summary, input.Description), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"task": taskData(r, created)})
}

func (h handler) readTask(w http.ResponseWriter, r *http.Request) {
	task, ok := h.state.Task(middleware.ResourceOwner(r), r.PathValue("task_guid"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"task": taskData(r, task)})
}

func (h handler) updateTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Task struct {
			Summary     *string `json:"summary"`
			Description *string `json:"description"`
			CompletedAt *string `json:"completed_at"`
		} `json:"task"`
		Update []string `json:"update_fields"`
	}
	if !read(w, r, &input, false) {
		return
	}
	var summary, description *string
	var completed *time.Time
	if len(input.Update) == 0 {
		fail(w, protocol.InvalidParameter)
		return
	}
	for _, key := range input.Update {
		switch key {
		case "summary":
			if input.Task.Summary == nil || strings.TrimSpace(*input.Task.Summary) == "" {
				fail(w, protocol.InvalidParameter)
				return
			}
			summary = input.Task.Summary
		case "description":
			if input.Task.Description == nil {
				fail(w, protocol.InvalidParameter)
				return
			}
			description = input.Task.Description
		case "completed_at":
			if input.Task.CompletedAt == nil {
				fail(w, protocol.InvalidParameter)
				return
			}
			ms, err := strconv.ParseInt(*input.Task.CompletedAt, 10, 64)
			if err != nil || ms < 0 || ms > 253402300799999 {
				fail(w, protocol.InvalidParameter)
				return
			}
			completed = new(time.Time)
			if ms > 0 {
				*completed = time.UnixMilli(ms).UTC()
			}
		default:
			fail(w, protocol.InvalidParameter)
			return
		}
	}
	changed, err := store.Commit(r.Context(), h.state, func() (store.Task, error) {
		return h.state.UpdateTask(middleware.ResourceOwner(r), r.PathValue("task_guid"), summary, description, completed)
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"task": taskData(r, changed)})
}
