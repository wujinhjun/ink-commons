//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Service) listRepositories(w http.ResponseWriter, r *http.Request) {
	repos, err := s.runtimeRepositories()
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, repos)
}

func (s *Service) registerRepository(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := decode(w, r, &raw); err != nil {
		fail(w, err)
		return
	}
	var name string
	if err := json.Unmarshal(raw["repo"], &name); err != nil {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidRepository(name) {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	delete(raw, "repo")
	values := ConfigValues(raw)
	if err := validateValues(values); err != nil {
		fail(w, apiError{400, StartupDiagnostic(err)})
		return
	}
	// Permission check uses the daemon's identity, never invoking shell credentials.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var pulls []Pull
	if _, err := s.up.gitHubGet(ctx, "/repos/"+name+"/pulls?state=open&per_page=1", &pulls); err != nil {
		fail(w, apiError{422, fmt.Sprintf("cannot read repository PRs: %s", redactedError(err))})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, created, err := s.registerConfig(name, values)
	if err != nil {
		fail(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	reply(w, status, repo)
}
