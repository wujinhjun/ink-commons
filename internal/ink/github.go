package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepository(name string) bool {
	return repoName.MatchString(name)
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type Repository struct {
	Repo           string           `json:"repo"`
	HumanDeveloper string           `json:"human_developer,omitempty"`
	ReviewerRoute  *execution.Route `json:"reviewer_route,omitempty"`
	ReviewerLogin  string           `json:"reviewer_login"`
}

type Config struct {
	Repositories []Repository
	PollSeconds  int
	GitHubToken  string
}

type upstream struct {
	token  string
	http   *http.Client
	github string
}

func newUpstream(token string) upstream {
	return upstream{
		token:  token,
		github: "https://api.github.com",
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (u upstream) gitHubGet(ctx context.Context, path string, result any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.github+path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream request")
	}
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream transport failure")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return nil, fmt.Errorf("invalid upstream response")
	}
	return res.Header, nil
}

func listPages[T any](ctx context.Context, u upstream, path string) ([]T, error) {
	var all []T
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	for page := 1; ; page++ {
		var items []T
		query := fmt.Sprintf("%s%sper_page=100&page=%d", path, separator, page)
		header, err := u.gitHubGet(ctx, query, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if !strings.Contains(header.Get("Link"), `rel="next"`) {
			return all, nil
		}
	}
}
