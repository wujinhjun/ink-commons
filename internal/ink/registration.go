package ink

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

func (s *Service) listRepositories(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, s.repositories())
}

func (s *Service) registerRepository(w http.ResponseWriter, r *http.Request) {
	var input Repository
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	input.Repo = strings.ToLower(strings.TrimSpace(input.Repo))
	if !ValidRepository(input.Repo) {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	defaults := s.config.RepositoryDefaults
	if input.ReviewerLogin == "" {
		input.ReviewerLogin = defaults.ReviewerLogin
	}
	if input.ReviewerCommand == "" {
		input.ReviewerCommand = defaults.ReviewerCommand
	}
	if input.ReviewerRoute == nil {
		input.ReviewerRoute = defaults.ReviewerRoute
	}
	if input.HumanDeveloper == "" {
		input.HumanDeveloper = defaults.HumanDeveloper
	}
	if input.PostApproval == nil {
		input.PostApproval = defaults.PostApproval
	}
	if strings.TrimSpace(input.ReviewerLogin) == "" {
		fail(w, apiError{400, "provide reviewer_login or configure repository_defaults.reviewer_login"})
		return
	}
	if err := input.PostApproval.Validate(); err != nil {
		fail(w, apiError{400, err.Error()})
		return
	}
	if input.ReviewerRoute != nil {
		if provider, ok := s.providers[input.ReviewerRoute.Provider].(interface{ ValidateDelivery() error }); ok {
			if err := provider.ValidateDelivery(); err != nil {
				fail(w, apiError{400, err.Error()})
				return
			}
		}
		if err := execution.ValidateDestination(s.providers, *input.ReviewerRoute, true); err != nil {
			fail(w, apiError{400, "invalid reviewer route: " + err.Error()})
			return
		}
	}
	// Only read PRs using the daemon's repository-scoped identity. No fallback to
	// the invoking shell's personal GitHub credentials.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var pulls []Pull
	if _, err := s.up.gitHubGet(ctx, "/repos/"+input.Repo+"/pulls?state=open&per_page=1", &pulls); err != nil {
		fail(w, apiError{422, fmt.Sprintf("cannot read repository PRs: %v", err)})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.repo(input.Repo); ok {
		if !reflect.DeepEqual(old, input) {
			fail(w, apiError{409, "repository already registered with different settings"})
			return
		}
		reply(w, 200, old)
		return
	}
	st, err := s.store.load()
	if err != nil {
		fail(w, err)
		return
	}
	st.Repositories = append(st.Repositories, input)
	if err := s.store.save(st); err != nil {
		fail(w, err)
		return
	}
	reply(w, 201, input)
}
