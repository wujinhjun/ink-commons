package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestReviewLoopAndHumanTakeover(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	sha3 := strings.Repeat("c", 40)
	var pull Pull
	payload := fmt.Sprintf(`{"number":42,"html_url":"https://github.com/acme/project/pull/42","state":"open","draft":true,"head":{"ref":"feat/export","sha":%q,"repo":{"full_name":"acme/project"}}}`, sha1)
	if err := json.Unmarshal([]byte(payload), &pull); err != nil {
		t.Fatal(err)
	}
	var reviews []Review
	var messages []map[string]string
	threads := 0
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/project/pulls":
			if pull.State == "closed" {
				reply(w, 200, []Pull{})
				return
			}
			reply(w, 200, []Pull{pull})
		case "/repos/acme/project/pulls/42":
			reply(w, 200, pull)
		case "/repos/acme/project/pulls/42/reviews":
			reply(w, 200, reviews)
		case "/channels/77/threads":
			threads++
			reply(w, 201, map[string]string{"id": "99"})
		case "/hook":
			var message map[string]string
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				t.Error(err)
			}
			messages = append(messages, message)
			reply(w, 200, map[string]string{"status": "accepted"})
		default:
			t.Errorf("unexpected upstream endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})

	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer-bot", ReviewerProject: "reviewer", ReviewChannel: "77"}
	cfg := Config{Repositories: []Repository{repo}, CCConnectURL: "https://stub.invalid/hook"}
	path := filepath.Join(t.TempDir(), "state.json")
	makeService := func() *Service {
		service, err := NewService(cfg, path)
		if err != nil {
			t.Fatal(err)
		}
		service.up.github, service.up.discord = "https://stub.invalid", "https://stub.invalid"
		service.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			mock.ServeHTTP(w, r)
			return w.Result(), nil
		})
		return service
	}
	s := makeService()
	request := func(method, path string, body []byte, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(body)))
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d, %s", method, path, w.Code, status, w.Body)
		}
		return w
	}
	work := func(phase string) Work {
		t.Helper()
		var w Work
		if err := json.Unmarshal(request("GET", "/v1/work/acme/project/42", nil, 200).Body.Bytes(), &w); err != nil {
			t.Fatal(err)
		}
		if w.Phase != phase {
			t.Fatalf("phase %s, want %s", w.Phase, phase)
		}
		return w
	}
	poll := func() {
		t.Helper()
		if err := s.pollRepo(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	addReview := func(id int64, sha, login, status string) {
		r := Review{ID: id, SHA: sha, State: status, URL: pull.URL + "#pullrequestreview-" + fmt.Sprint(id)}
		r.User.Login = login
		reviews = append(reviews, r)
	}

	poll()
	work("unclaimed")
	pushes, err := ReadPush("git@github.com:Acme/Project.git", Route{Project: "developer", SessionKey: "discord:11"}, strings.NewReader("refs/heads/local "+sha1+" refs/heads/feat/export "+strings.Repeat("0", 40)+"\n"))
	if err != nil || len(pushes) != 1 {
		t.Fatalf("parsed pushes %v: %v", pushes, err)
	}
	data, err := json.Marshal(pushes)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", "/v1/pushes", data, 202)
	poll()
	if w := work("draft"); w.Origin == nil || w.Origin.SessionKey != "discord:11" || threads != 0 {
		t.Fatalf("draft routing: %+v", w)
	}

	pull.Draft = false
	poll()
	if w := work("review_pending"); w.Task.Delivery != "accepted" || w.ReviewThread != "99" || len(messages) != 1 || messages[0]["session_key"] != "discord:99" {
		t.Fatalf("review dispatch: %+v, %v", w, messages)
	}

	s = makeService()
	poll()
	if threads != 1 || len(messages) != 1 {
		t.Fatal("restart repeated review delivery")
	}
	addReview(1, sha1, "someone-else", "APPROVED")
	addReview(2, sha2, "reviewer-bot", "APPROVED")
	poll()
	work("review_pending")
	addReview(3, sha1, "reviewer-bot", "CHANGES_REQUESTED")
	poll()
	if w := work("changes_requested"); w.Task.Delivery != "accepted" || len(messages) != 2 || messages[1]["session_key"] != "discord:11" {
		t.Fatalf("developer dispatch: %+v, %v", w, messages)
	}

	request("POST", "/v1/work/acme/project/42/assign", []byte(`{"role":"developer","github_login":"human-dev"}`), 200)
	w := work("changes_requested")
	if w.Task.Delivery != "manual" || w.Developer.Route != nil || w.Origin == nil || w.Origin.SessionKey != "discord:11" {
		t.Fatalf("developer takeover: %+v", w)
	}
	pull.Head.SHA = sha3
	poll()
	if w := work("review_pending"); w.Task.SHA != sha3 || threads != 1 || len(messages) != 3 || messages[2]["session_key"] != "discord:99" {
		t.Fatalf("re-review: %+v, %v", w, messages)
	}

	request("POST", "/v1/work/acme/project/42/assign", []byte(`{"role":"reviewer","github_login":"human-reviewer"}`), 200)
	if w := work("review_pending"); w.Task.Delivery != "manual" {
		t.Fatalf("reviewer takeover: %+v", w)
	}
	addReview(4, sha3, "reviewer-bot", "APPROVED")
	poll()
	work("review_pending")
	addReview(5, sha3, "human-reviewer", "APPROVED")
	poll()
	work("approved")
	pull.State, pull.Merged = "closed", true
	poll()
	work("merged")
	if threads != 1 || len(messages) != 3 {
		t.Fatal("manual assignments triggered bot deliveries")
	}
}

func TestAmbiguousOrigin(t *testing.T) {
	var pull Pull
	pull.Head.SHA, pull.Head.Ref = strings.Repeat("a", 40), "feature"
	pull.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{"acme/project"}
	first := Push{Repo: "acme/project", Ref: pull.Head.Ref, SHA: pull.Head.SHA, Route: Route{"dev", "discord:11"}}
	second := first
	second.Route.SessionKey = "discord:12"
	if matchOrigin([]Push{first, second}, pull) != nil {
		t.Fatal("matched ambiguous origin")
	}
	second.Repo = "fork/project"
	if route := matchOrigin([]Push{first, second}, pull); route == nil || *route != first.Route {
		t.Fatal("did not match source repo")
	}
}
