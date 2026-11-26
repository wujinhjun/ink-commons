// The demo uses only the standard library so the handler can be copied into
// a separate module. It acknowledges deliveries without launching an agent.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

type request struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Route     struct {
		Provider string `json:"provider"`
		Target   string `json:"target"`
		Session  string `json:"session"`
	} `json:"route"`
	Task struct {
		ID        string `json:"task_id"`
		SHA       string `json:"sha"`
		AttemptID string `json:"attempt_id"`
		Attempt   int    `json:"attempt"`
	} `json:"task"`
}

func execute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var req request
	if decoder.Decode(&req) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF ||
		req.Version != 1 || req.Route.Provider == "" || req.Route.Target == "" || req.Task.ID == "" || req.Task.SHA == "" {
		http.Error(w, "invalid adapter request", http.StatusBadRequest)
		return
	}
	result := make(map[string]string, 1)
	switch req.Operation {
	case "prepare":
		digest := sha256.Sum256([]byte(req.Route.Provider + "\x00" + req.Route.Target + "\x00" + req.Task.ID))
		result["session"] = fmt.Sprintf("demo-%x", digest[:8])
	case "deliver":
		if req.Route.Session == "" || req.Task.AttemptID == "" || req.Task.Attempt <= 0 {
			http.Error(w, "resolved session and attempt required", http.StatusBadRequest)
			return
		}
		slog.Info("demo delivery accepted", "attempt", req.Task.Attempt)
		result["status"] = "accepted"
	default:
		http.Error(w, "unsupported operation", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func main() {
	port := flag.String("port", "8789", "loopback HTTP port")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/execute", execute)
	server := http.Server{
		Addr: net.JoinHostPort("127.0.0.1", *port), Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	}
	slog.Info("demo executor listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		slog.Error("demo listener failed")
		os.Exit(1)
	}
}
