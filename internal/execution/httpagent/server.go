package httpagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type HandlerFunc func(context.Context, Request) (Response, error)

// Handler exposes only the outbound adapter endpoint. Error bodies are fixed
// so upstream responses and credentials cannot reach the daemon or its logs.
func Handler(token string, handle HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "adapter authorization required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		var req Request
		if decoder.Decode(&req) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF || !validRequest(req) {
			http.Error(w, "invalid adapter request", http.StatusBadRequest)
			return
		}
		res, err := handle(r.Context(), req)
		if err != nil {
			var status Problem
			if !errors.As(err, &status) {
				status = http.StatusBadGateway
			}
			http.Error(w, http.StatusText(int(status)), int(status))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
}

type Problem int

func (p Problem) Error() string { return http.StatusText(int(p)) }

func validRequest(r Request) bool {
	if r.Version != 1 || strings.TrimSpace(r.Operation) == "" {
		return false
	}
	if !r.Route.Valid(r.Operation == "prepare" || r.Operation == "placement") {
		return false
	}
	if r.Operation == "notify" {
		n := r.Notification
		return r.Task == nil && r.Allocation == nil && r.Mode == "" && r.Before == "" && r.Reason == "" &&
			r.RequireACK != nil && n != nil && n.EventID != "" && n.Event != "" && n.Repository != "" && n.Number > 0 && n.SHA != ""
	}
	if r.Notification != nil || r.RequireACK != nil {
		return false
	}
	switch r.Operation {
	case "deliver", "status", "stop":
		if r.Task == nil || r.Task.Attempt <= 0 || r.Task.AttemptID == "" {
			return false
		}
	case "prepare":
		if r.Allocation != nil {
			legacy := r.Mode == "find" && r.Allocation.ID == "" && r.Allocation.Name != ""
			modern := r.Allocation.ID != "" && r.Allocation.Name == ""
			if (!legacy && !modern) || strings.TrimSpace(r.Allocation.Parent) == "" || r.Allocation.Parent != r.Route.Parent ||
				r.Allocation.ReservedAt == nil || r.Allocation.ReservedAt.IsZero() || (r.Mode != "create" && r.Mode != "find") {
				return false
			}
		} else if r.Mode != "" {
			return false
		}
	case "conversation_context":
		if r.Task == nil || r.Task.ContextBefore == "" || r.Task.Source == nil || *r.Task.Source != r.Route {
			return false
		}
	}
	if (r.Operation != "prepare" && (r.Allocation != nil || r.Mode != "")) ||
		(r.Operation != "conversation_context" && r.Before != "") ||
		(r.Operation != "blocked_notice" && r.Reason != "") {
		return false
	}
	task := r.Task
	return task != nil && task.TaskID != "" && task.SHA != "" && task.Repository != "" && task.Number > 0 &&
		(task.Role == "reviewer" || task.Role == "developer")
}
