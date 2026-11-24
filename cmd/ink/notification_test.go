package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotificationAckUsesLocalDaemonAndPrintsDeduplicationReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"first", `{"status":"acknowledged","first":true}`, 200, true},
		{"duplicate", `{"status":"acknowledged","first":false}`, 200, true},
		{"stale", `{"status":"superseded","first":false}`, 200, true},
		{"missing", `{"error":"notification event not found"}`, 404, false},
		{"deployment gate", `{"error":"deployment verification in progress"}`, 503, false},
		{"bad receipt", `{"status":"accepted"}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "ink-ack-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			t.Setenv("INK_DATA_DIR", dir)
			ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					EventID string `json:"event_id"`
				}
				if r.Method != "POST" || r.URL.Path != "/v1/notifications/ack" || json.NewDecoder(r.Body).Decode(&input) != nil || input.EventID != "acme/project#42:abc:review_approved" {
					t.Error("wrong notification receipt request")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})}
			go server.Serve(ln)
			defer server.Close()
			out, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			original := os.Stdout
			os.Stdout = out
			err = run([]string{"notification", "ack", "--event", "acme/project#42:abc:review_approved"})
			os.Stdout = original
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected result: %v", err)
			}
			body, _ := os.ReadFile(out.Name())
			if tc.valid && strings.TrimSpace(string(body)) != tc.body {
				t.Fatalf("receipt output: %s", body)
			}
			if !tc.valid && len(body) > 0 {
				t.Fatalf("failed command printed successful receipt: %s", body)
			}
		})
	}
}

func TestNotificationAckRequiresEvent(t *testing.T) {
	for _, args := range [][]string{{"notification"}, {"notification", "ack"}, {"notification", "ack", "--event", "x", "extra"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}

func TestNotificationStatusUsesReadOnlyEscapedEventQuery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		valid      bool
	}{
		{"merged", `{"event_id":"acme/project#42:abc:review_approved","event":"review_approved","sha":"abc","current_head_sha":"abc","phase":"approved","notification_state":"acknowledged","merge":{"auto_merge_enabled":true,"state":"merged","merge_sha":"def"}}`, 200, true},
		{"missing", `{}`, 404, false},
		{"bad JSON", `bad`, 200, false},
		{"missing observation", `{"event_id":"acme/project#42:abc:review_approved"}`, 200, false},
		{"wrong event", `{"event_id":"other","merge":{"state":"merged"}}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "ink-status-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			t.Setenv("INK_DATA_DIR", dir)
			ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/v1/notifications/status" || r.URL.Query().Get("event_id") != "acme/project#42:abc:review_approved" {
					t.Error("status request changed method or lost # in event ID")
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			})}
			go server.Serve(ln)
			defer server.Close()
			out, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			original := os.Stdout
			os.Stdout = out
			err = run([]string{"notification", "status", "--event", "acme/project#42:abc:review_approved"})
			os.Stdout = original
			if (err == nil) != tc.valid || calls != 1 {
				t.Fatalf("unexpected status result calls=%d err=%v", calls, err)
			}
			body, _ := os.ReadFile(out.Name())
			if tc.valid && !strings.Contains(string(body), `"merge_sha":"def"`) {
				t.Fatalf("lost merge observation: %s", body)
			}
			if !tc.valid && len(body) != 0 {
				t.Fatal("failure printed successful status")
			}
		})
	}
}
