package ink

import (
	"context"
	"encoding/json"
	"ink-commons/internal/execution"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Existing workflow regressions change central policy through this helper
// instead of editing the daemon's obsolete JSON import input in memory.
func setRepositoryConfigForTest(t *testing.T, s *Service, repo Repository) {
	t.Helper()
	tx, err := s.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		t.Fatal(err)
	}
	global := recordMap(records)[""]
	global.Revision++
	if err := putConfigRecord(tx, global); err != nil {
		t.Fatal(err)
	}
	if err := putConfigRecord(tx, ConfigRecord{Scope: repo.Repo, Values: legacyRepositoryValues(repo), Revision: global.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigInheritanceNullFalseAndValidation(t *testing.T) {
	global := GlobalConfig{"reviewer_login": jsonValue("first"), "reviewer_route": jsonValue(execution.Route{Provider: "test", Target: "a", Session: "old"}), "auto_merge": json.RawMessage(`{"enabled":true,"method":"squash"}`), "execution.max_attempts": jsonValue(5)}
	effective, err := resolveConfig("acme/project", global, RepositoryOverride{"auto_merge": json.RawMessage(`{"enabled":false}`), "reviewer_route": jsonValue(nil), "reviewer_command": jsonValue("")}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Repository.ReviewerRoute != nil || effective.Repository.AutoMerge.Enabled || effective.Repository.AutoMerge.Method != "" || effective.Execution.MaxAttempts != 5 {
		t.Fatalf("incorrect atomic overrides: %+v", effective)
	}
	if effective.Sources["auto_merge"] != "repository" || effective.Sources["reviewer_login"] != "global" || effective.Sources["execution.ack_seconds"] != "builtin" {
		t.Fatal(effective.Sources)
	}
	inherited, err := resolveConfig("acme/project", global, nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Repository.ReviewerRoute.Session != "old" {
		t.Fatal("missing key did not inherit")
	}
	for key, raw := range map[string]string{"execution.max_attempts": "0", "execution.ack_seconds": "null", "reviewer_login": "false", "reviewer_route": `{"provider":"test","unknown":true}`, "auto_merge": `{"enabled":null}`, "postApproval": `{"command":[null]}`, "task_mode": `""`, "unknown": "true"} {
		if _, err := resolveConfig("", nil, RepositoryOverride{key: json.RawMessage(raw)}, 1); err == nil {
			t.Errorf("accepted %s=%s", key, raw)
		}
	}
}

func configAPI(t *testing.T, s *Service, method, scope, key, value string, want int) *httptest.ResponseRecorder {
	t.Helper()
	query := url.Values{"repo": {scope}, "key": {key}}
	body := ""
	if method == "POST" {
		body = `{"value":` + value + `}`
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest(method, "/v1/config?"+query.Encode(), strings.NewReader(body)))
	if out.Code != want {
		t.Fatalf("config %s %s %s: %d %s", method, scope, key, out.Code, out.Body)
	}
	return out
}

func registerConfigAPI(t *testing.T, s *Service, body string, want int) {
	t.Helper()
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/repositories", strings.NewReader(body)))
	if out.Code != want {
		t.Fatalf("register: %d %s", out.Code, out.Body)
	}
}

func centralConfigService(t *testing.T, cfg Config, providers map[string]execution.Executor) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), stateDatabase)
	s, err := NewService(cfg, path, providers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.up.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		out := httptest.NewRecorder()
		reply(out, 200, []Pull{})
		return out.Result(), nil
	})
	return s, path
}

func TestCentralConfigUpdatesInheritanceRestartAndSnapshotIsolation(t *testing.T) {
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "first"}}
	s, path := centralConfigService(t, cfg, nil)
	registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	} // stale workflow snapshot
	configAPI(t, s, "POST", "", "reviewer_login", `"second"`, 200)
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	effective, err := s.store.effectiveConfig("acme/project")
	if err != nil {
		t.Fatal(err)
	}
	if effective.Repository.ReviewerLogin != "second" {
		t.Fatal("workflow save overwrote configuration")
	}
	records, _ := s.store.configRecords()
	if len(recordMap(records)["acme/project"].Values) != 0 {
		t.Fatal("registration expanded defaults")
	}
	configAPI(t, s, "POST", "acme/project", "reviewer_login", `"second"`, 200)
	configAPI(t, s, "POST", "", "reviewer_login", `"third"`, 200)
	effective, _ = s.store.effectiveConfig("acme/project")
	if effective.Repository.ReviewerLogin != "second" {
		t.Fatal("equal-valued explicit override was pruned")
	}
	configAPI(t, s, "DELETE", "acme/project", "reviewer_login", "", 200)
	effective, _ = s.store.effectiveConfig("acme/project")
	if effective.Repository.ReviewerLogin != "third" {
		t.Fatal("unset did not inherit")
	}
	configAPI(t, s, "POST", "", "postApproval", `{"command":["/bin/true"]}`, 200)
	configAPI(t, s, "POST", "acme/project", "postApproval", `null`, 200)
	configAPI(t, s, "POST", "", "auto_merge", `{"enabled":true}`, 200)
	configAPI(t, s, "POST", "acme/project", "auto_merge", `{"enabled":false}`, 200)
	effective, _ = s.store.effectiveConfig("acme/project")
	if effective.Repository.PostApproval != nil || effective.Repository.AutoMerge.Enabled {
		t.Fatal("null/false lost")
	}
	configAPI(t, s, "POST", "", "execution.max_attempts", "0", 400)
	configAPI(t, s, "POST", "", "poll_seconds", "10", 400)
	configAPI(t, s, "DELETE", "", "reviewer_login", "", 400) // would invalidate registered repository
	configAPI(t, s, "POST", "acme/missing", "reviewer_login", `"other"`, 404)
	raw := configAPI(t, s, "GET", "acme/project", "postApproval", "", 200)
	if !strings.Contains(raw.Body.String(), `"present":true`) || !strings.Contains(raw.Body.String(), `"value":null`) {
		t.Fatal(raw.Body)
	}
	before := effective.Revision
	s.Close()
	cfg.RepositoryDefaults.ReviewerLogin = "old-json-must-not-return"
	restarted, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	effective, err = restarted.store.effectiveConfig("acme/project")
	if err != nil {
		t.Fatal(err)
	}
	if effective.Repository.ReviewerLogin != "third" || effective.Repository.PostApproval != nil || effective.Revision != before {
		t.Fatalf("restart changed runtime settings: %+v", effective)
	}
}

func TestCentralMigrationPreservesLegacyBehaviorAndExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateDatabase)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := state{Work: map[string]*Work{}, Repositories: []Repository{{Repo: "acme/old", ReviewerLogin: "old"}, {Repo: "acme/conflict", ReviewerLogin: "sqlite"}}}
	if err := store.save(old); err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "global", PostApproval: &ApprovalHook{Command: []string{"/bin/true"}}}, Repositories: []Repository{{Repo: "acme/conflict", ReviewerLogin: "json"}}}
	s, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := s.store.effectiveConfig("acme/old")
	if err != nil {
		t.Fatal(err)
	}
	if effective.Repository.PostApproval != nil || effective.Repository.ReviewerLogin != "old" {
		t.Fatal("legacy absence unexpectedly inherited")
	}
	conflict, _ := s.store.effectiveConfig("acme/conflict")
	if conflict.Repository.ReviewerLogin != "json" {
		t.Fatal("migration changed old precedence")
	}
	legacy, _ := s.store.load()
	if legacy.Repositories[1].ReviewerLogin != "sqlite" && legacy.Repositories[0].ReviewerLogin != "sqlite" {
		t.Fatal("shadowed original was not retained")
	}
	configAPI(t, s, "POST", "", "reviewer_login", `"new-global"`, 200)
	configAPI(t, s, "DELETE", "acme/old", "reviewer_login", "", 200)
	s.Close()
	output := filepath.Join(t.TempDir(), "export.json")
	if err := StateSnapshot(dir, output, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	restoredDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(restoredDir, legacyState), data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := NewService(Config{}, filepath.Join(restoredDir, stateDatabase), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	effective, err = restored.store.effectiveConfig("acme/old")
	if err != nil {
		t.Fatal(err)
	}
	if effective.Repository.ReviewerLogin != "new-global" || effective.Repository.PostApproval != nil {
		t.Fatal("export/import lost overrides")
	}
	records, _ := restored.store.configRecords()
	if _, ok := recordMap(records)[legacyConfigScope]; !ok {
		t.Fatal("JSON import archive lost")
	}
}

func TestTaskConfigFreezesFirstDispatchAndRetryButRefreshesNewSHA(t *testing.T) {
	executor := &testExecutor{}
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "first", ReviewerRoute: &execution.Route{Provider: "test", Target: "first"}}}
	s, _ := centralConfigService(t, cfg, map[string]execution.Executor{"test": executor})
	registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
	p := Pull{Number: 42, State: "open", URL: "https://github.com/acme/project/pull/42"}
	p.Head.SHA = strings.Repeat("a", 40)
	repo, _ := s.repo("acme/project")
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := s.store.load()
	w := st.Work[key(repo.Repo, 42)]
	w.Linked = true
	w.derive()
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	configAPI(t, s, "POST", "", "reviewer_login", `"second"`, 200)
	configAPI(t, s, "POST", "", "execution.max_attempts", "5", 200)
	// No extra GitHub observation is needed between a config update and dispatch.
	if err := s.dispatch(context.Background(), repo, 42); err != nil {
		t.Fatal(err)
	}
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	taskID := w.Task.ID
	if w.Task.ConfigSnapshot == nil || w.Task.ConfigSnapshot.Execution.MaxAttempts != 5 || executor.deliveries[0].task.GitHubLogin != "second" {
		t.Fatalf("first attempt used stale config: %+v", w.Task)
	}
	frozen, _ := json.Marshal(w.Task.ConfigSnapshot)
	configAPI(t, s, "POST", "", "reviewer_login", `"third"`, 200)
	configAPI(t, s, "POST", "", "reviewer_route", `{"provider":"test","target":"third"}`, 200)
	configAPI(t, s, "POST", "", "execution.max_attempts", "1", 200)
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	w.Task.Runs[0].State = "failed"
	w.Task.Delivery = "pending"
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatch(context.Background(), repo, 42); err != nil {
		t.Fatal(err)
	}
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	after, _ := json.Marshal(w.Task.ConfigSnapshot)
	if w.Task.ID != taskID || string(frozen) != string(after) || executor.deliveries[1].task.GitHubLogin != "second" || executor.deliveries[1].route.Target != "first" {
		t.Fatal("retry changed frozen configuration")
	}
	// A new default review must not approve an already-dispatched round.
	if err := s.observe(repo, p, []Review{reviewFor(9, p.Head.SHA, "third", "APPROVED")}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.store.load()
	if st.Work[key(repo.Repo, 42)].Phase != "review_pending" {
		t.Fatal("new default reinterpreted old review")
	}
	w = st.Work[key(repo.Repo, 42)]
	for i := range w.Task.Runs {
		w.Task.Runs[i].State = "failed"
	}
	s.store.save(st)
	p.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatch(context.Background(), repo, 42); err != nil {
		t.Fatal(err)
	}
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	if w.Task.ID == taskID || w.Task.ConfigSnapshot.Execution.MaxAttempts != 1 || w.Reviewer.GitHubLogin != "third" || executor.deliveries[2].route.Target != "third" || executor.prepared != 2 {
		t.Fatalf("new SHA did not refresh route and policy: %+v", w.Task)
	}
}

func TestEffectivePRConfigShowsExplicitSources(t *testing.T) {
	s, _ := centralConfigService(t, Config{RepositoryDefaults: Repository{ReviewerLogin: "global"}}, nil)
	registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
	p := Pull{Number: 42, State: "open"}
	p.Head.SHA = strings.Repeat("a", 40)
	repo, _ := s.repo("acme/project")
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := s.store.load()
	w := st.Work[key(repo.Repo, 42)]
	if err := applyChange(w, "link", workChange{GitHubLogin: "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := applyChange(w, "assign", workChange{Role: "reviewer", GitHubLogin: "explicit"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/config?repo=acme/project&pr=42&effective=true", nil))
	var effective EffectiveConfig
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &effective) != nil {
		t.Fatal(out.Code, out.Body)
	}
	if effective.Repository.ReviewerLogin != "explicit" || effective.Sources["reviewer_login"] != "pr" || effective.Sources["human_developer"] != "pr" {
		t.Fatal(out.Body)
	}
}

func TestCooperativeConfigSnapshotIsDurableBeforeWakeAndClaim(t *testing.T) {
	f := newHandoffFixture(t)
	configAPI(t, f.s, "POST", f.repo.Repo, "execution.max_attempts", "7", 200)
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	w := f.work(t)
	if w.Task.ConfigSnapshot == nil || w.Task.ConfigSnapshot.Execution.MaxAttempts != 7 || w.Task.Handoff.Wake.Attempts != 1 {
		t.Fatal("wake did not persist configuration")
	}
	task := w.Task
	configAPI(t, f.s, "POST", f.repo.Repo, "execution.max_attempts", "1", 200)
	f.restart(t)
	f.claim(t, task, claimA, 200)
	if f.work(t).Task.ConfigSnapshot.Execution.MaxAttempts != 7 {
		t.Fatal("claim replaced wake snapshot")
	}

	fresh := newHandoffFixture(t)
	configAPI(t, fresh.s, "POST", fresh.repo.Repo, "execution.max_attempts", "8", 200)
	fresh.claim(t, fresh.work(t).Task, claimA, 200)
	if fresh.work(t).Task.ConfigSnapshot == nil || fresh.work(t).Task.ConfigSnapshot.Execution.MaxAttempts != 8 {
		t.Fatal("direct claim did not freeze configuration")
	}
}

func TestConfigMutationRollbackAndCorruptionFailClosed(t *testing.T) {
	s, path := centralConfigService(t, Config{RepositoryDefaults: Repository{ReviewerLogin: "reviewer"}}, nil)
	registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
	before, _ := s.store.effectiveConfig("acme/project")
	if _, err := s.store.db.Exec(`CREATE TRIGGER reject_config BEFORE UPDATE ON runtime_config WHEN NEW.scope='acme/project' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	configAPI(t, s, "POST", "acme/project", "reviewer_login", `"other"`, 500)
	after, _ := s.store.effectiveConfig("acme/project")
	if before.Revision != after.Revision || after.Repository.ReviewerLogin != "reviewer" {
		t.Fatal("failed write partially committed")
	}
	if _, err := s.store.db.Exec(`UPDATE runtime_config SET payload='null' WHERE scope=''`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if restarted, err := NewService(Config{}, path, nil); err == nil {
		restarted.Close()
		t.Fatal("corrupted runtime configuration was reimported from JSON")
	}
}

func TestEqualReviewerAssignmentStillPinsPROverride(t *testing.T) {
	s, _ := centralConfigService(t, Config{RepositoryDefaults: Repository{ReviewerLogin: "original"}}, nil)
	registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
	p := Pull{Number: 42, State: "open"}
	p.Head.SHA = strings.Repeat("a", 40)
	repo, _ := s.repo("acme/project")
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := s.store.load()
	w := st.Work[key(repo.Repo, 42)]
	if err := applyChange(w, "link", workChange{GitHubLogin: "dev"}); err != nil {
		t.Fatal(err)
	}
	taskID := w.Task.ID
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/work/acme/project/42/assign", strings.NewReader(`{"role":"reviewer","github_login":"original"}`)))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	if w.ReviewerOverride == nil || w.Task.ID != taskID {
		t.Fatal("equal assignment reset task or failed to pin override")
	}
	configAPI(t, s, "POST", "", "reviewer_login", `"changed-global"`, 200)
	p.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = s.store.load()
	w = st.Work[key(repo.Repo, 42)]
	if w.Reviewer.GitHubLogin != "original" || w.Task.ConfigSnapshot.Sources["reviewer_login"] != "pr" {
		t.Fatal("explicit equal-valued assignment started inheriting")
	}
}
