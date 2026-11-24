package httpagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func TestAuthenticatedStatusAndStop(t *testing.T) {
	operations := []string{}
	d := execution.Delivery{TaskID: "task", AttemptID: "attempt", Attempt: 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer executor-secret" {
			t.Error("missing authenticated source")
		}
		var req struct {
			Operation string             `json:"operation"`
			Task      execution.Delivery `json:"task"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Task.TaskID != d.TaskID || req.Task.AttemptID != d.AttemptID || req.Task.Attempt != 2 {
			t.Error("missing execution round")
		}
		operations = append(operations, req.Operation)
		json.NewEncoder(w).Encode(map[string]any{"receipt": execution.Receipt{TaskID: d.TaskID, AttemptID: d.AttemptID, Sequence: 4, State: "stopped"}})
	}))
	defer server.Close()
	e, err := NewManaged(server.Client(), server.URL, "executor-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"status", "stop"} {
		var r execution.Receipt
		if operation == "status" {
			r, err = e.Status(context.Background(), execution.Route{}, d)
		} else {
			r, err = e.Stop(context.Background(), execution.Route{}, d)
		}
		if err != nil || r.AttemptID != d.AttemptID || r.Sequence != 4 {
			t.Fatal(r, err)
		}
	}
	if len(operations) != 2 || operations[0] != "status" || operations[1] != "stop" {
		t.Fatal(operations)
	}
}

func TestLegacyEndpointReceivesNoMonitoringRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("legacy endpoint received execution operation") }))
	defer server.Close()
	e, err := New(server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Status(context.Background(), execution.Route{}, execution.Delivery{}); err == nil {
		t.Fatal("legacy monitoring was enabled")
	}
	if _, err = e.Stop(context.Background(), execution.Route{}, execution.Delivery{}); err == nil {
		t.Fatal("legacy stop was enabled")
	}
}
