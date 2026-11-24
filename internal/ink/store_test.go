//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

func newFixtureService(t *testing.T, config Config, path string, providers map[string]execution.Executor) (*Service, error) {
	t.Helper()
	if err := createStateDatabase(path); err != nil {
		t.Fatal(err)
	}
	return NewService(config, path, providers)
}

type workFixture struct{ Work map[string]*Work }

func loadFixture(s *stateStore) (workFixture, error) {
	st := workFixture{Work: make(map[string]*Work)}
	works, err := s.listWork()
	for _, w := range works {
		st.Work[keyForWork(w)] = w
	}
	return st, err
}

func saveFixture(s *stateStore, st workFixture) error {
	for _, w := range st.Work {
		if err := s.saveWork(w); err != nil {
			return err
		}
	}
	return nil
}

func testStore(t *testing.T, path string) *stateStore {
	t.Helper()
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func initTestStore(t *testing.T) (*stateStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := InitializeState(dir); err != nil {
		t.Fatal(err)
	}
	return testStore(t, filepath.Join(dir, stateDatabase)), dir
}

func pollIncludes(t *testing.T, s *Service, repo string, number int) bool {
	t.Helper()
	numbers, err := statedb.New(s.store.db).ListPollPending(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(numbers, int64(number))
}

func TestServeRefusesMissingState(t *testing.T) {
	parent := t.TempDir()
	empty := filepath.Join(parent, "empty")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{empty, filepath.Join(parent, "wrong-path")} {
		err := Serve(context.Background(), Config{}, dir, nil)
		if err == nil || !strings.Contains(err.Error(), "ink state init") {
			t.Fatalf("serve with missing state: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, stateDatabase)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("serve created database: %v", err)
		}
	}
}

func TestStateSchemaRejectsUnsupportedVersions(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3, 4, 5, 6, 8} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s, dir := initTestStore(t)
			if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
				t.Fatal(err)
			}
			s.db.Close()
			path := filepath.Join(dir, stateDatabase)
			if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("version %d (expected 7)", version)) {
				t.Fatalf("open unsupported version: %v", err)
			}
			db, err := openSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var actual int
			if err := db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != version {
				t.Fatalf("schema was changed: %d, %v", actual, err)
			}
		})
	}
}

func TestExplicitInitRefusesExistingStateAndEmptyPlaceholders(t *testing.T) {
	s, dir := initTestStore(t)
	s.db.Close()
	path := filepath.Join(dir, stateDatabase)
	if err := InitializeState(dir); err == nil {
		t.Fatal("reinitialized existing database")
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "version 0") {
		t.Fatalf("opened empty file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("changed placeholder: %v, %v", info, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	marker := path + ".initialized"
	if err := os.WriteFile(marker, []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeState(dir); err == nil {
		t.Fatal("initialized in a previous instance's directory")
	}
}

func TestStateDatabaseMustBePrivateAndComplete(t *testing.T) {
	s, dir := initTestStore(t)
	if _, err := s.db.Exec("DROP TABLE pushes"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	path := filepath.Join(dir, stateDatabase)
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "pushes") {
		t.Fatalf("opened incomplete schema: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("opened public database: %v", err)
	}
}

func TestStateSnapshotPreservesAggregateAndIndexes(t *testing.T) {
	s, dir := initTestStore(t)
	work := &Work{Repo: "acme/project", Pull: Pull{Number: 7, State: "closed", Merged: true}, Phase: "merged",
		History:       []*Task{{ID: "old-task", Handoff: &TaskHandoff{State: "submitted"}}},
		Notifications: []NotificationRun{{Notification: execution.Notification{EventID: "event"}, RequireACK: true, State: "awaiting_ack"}},
	}
	if err := s.saveWork(work); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "backup")
	lock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := StateSnapshot(dir, output, false); err == nil {
		t.Fatal("backup raced a held daemon lock")
	}
	lock.Close()
	s.db.Close()
	if err := StateSnapshot(dir, output, false); err != nil {
		t.Fatal(err)
	}
	snapshot := testStore(t, output)
	w, task, err := snapshot.loadTask("old-task")
	if err != nil || task == nil || task.Handoff.State != "submitted" || w.Pull.Number != 7 {
		t.Fatalf("backup lost handoff: %v, %v", task, err)
	}
	w, err = snapshot.loadNotification("event")
	if err != nil || w == nil || w.Notifications[0].State != "awaiting_ack" {
		t.Fatalf("backup lost receipt: %v, %v", w, err)
	}
	output += ".json"
	if err := StateSnapshot(dir, output, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var export stateExport
	if err := json.Unmarshal(data, &export); err != nil || export.Work[keyForWork(work)] == nil {
		t.Fatalf("export lost aggregate: %s, %v", data, err)
	}
}

func TestSaveWorkFencesStaleWriterAndRollsBackIndexFailure(t *testing.T) {
	s, _ := initTestStore(t)
	w := &Work{Repo: "acme/project", Pull: Pull{Number: 7, State: "open"}, Task: &Task{ID: "current"}}
	if err := s.saveWork(w); err != nil {
		t.Fatal(err)
	}
	stale, err := s.loadWork(keyForWork(w))
	if err != nil {
		t.Fatal(err)
	}
	w.Task.ID = "successor"
	w.History = []*Task{{ID: "current"}}
	if err := s.saveWork(w); err != nil {
		t.Fatal(err)
	}
	if err := s.saveWork(stale); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("accepted stale write: %v", err)
	}
	w.Pull.State = "closed"
	w.History = nil
	w.Notifications = []NotificationRun{
		{Notification: execution.Notification{EventID: "duplicate"}},
		{Notification: execution.Notification{EventID: "duplicate"}},
	}
	if err := s.saveWork(w); err == nil {
		t.Fatal("duplicate event committed")
	}
	for _, id := range []string{"current", "successor"} {
		found, task, err := s.loadTask(id)
		if err != nil || task == nil || found.Pull.State != "open" {
			t.Fatalf("failed transaction changed payload or task index for %s: %v, %v", id, task, err)
		}
	}
	if found, err := s.loadNotification("duplicate"); err != nil || found != nil {
		t.Fatalf("failed transaction published event: %v, %v", found, err)
	}
	if !pollIncludes(t, &Service{store: s}, w.Repo, w.Pull.Number) {
		t.Fatal("failed transaction changed pending index")
	}
}

func TestIndexedOriginIgnoresExpiredAndOtherRepositories(t *testing.T) {
	s, _ := initTestStore(t)
	now := time.Now()
	var pull Pull
	pull.Head.SHA, pull.Head.Ref = strings.Repeat("a", 40), "feature"
	pull.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{"Acme/Project"}
	first := Push{Repo: "acme/project", Ref: pull.Head.Ref, SHA: pull.Head.SHA, At: now, Route: execution.Route{Provider: "test", Target: "dev", Session: "dev-session"}}
	second := first
	second.Route.Session = "other-session"
	second.At = now.Add(-time.Hour)
	other := second
	other.Repo = "fork/project"
	if err := s.savePushes([]Push{first, second, other, first}, now); err != nil {
		t.Fatal(err)
	}
	if origin, err := s.matchOrigin(pull, now); err != nil || origin != nil {
		t.Fatalf("matched ambiguous origin: %v, %v", origin, err)
	}
	later := now.Add(7*24*time.Hour - time.Minute)
	if origin, err := s.matchOrigin(pull, later); err != nil || origin == nil || *origin != first.Route {
		t.Fatalf("indexed lookup failed after conflicting observation expired: %v, %v", origin, err)
	}
	if err := s.savePushes(nil, later); err != nil {
		t.Fatal(err)
	}
	rows, err := statedb.New(s.db).ListPushes(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("prune did not remove expired observations: %v, %v", rows, err)
	}
}

func TestPublishFallbackPreservesBytesPermissionsAndNoOverwrite(t *testing.T) {
	for _, unsupported := range []error{syscall.ENOTSUP, syscall.EPERM, syscall.EXDEV, syscall.ENOSYS} {
		t.Run(unsupported.Error(), func(t *testing.T) {
			dir := t.TempDir()
			source, destination := filepath.Join(dir, "source"), filepath.Join(dir, "backup")
			data := bytes.Repeat([]byte("current database snapshot\n"), 1024)
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			noLink := func(string, string) error { return unsupported }
			if err := publishFileWithLink(source, destination, noLink); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("fallback backup changed")
			}
			info, err := os.Stat(destination)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("fallback backup not private")
			}
			if err := os.WriteFile(source, []byte("new data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := publishFileWithLink(source, destination, noLink); !errors.Is(err, os.ErrExist) {
				t.Fatalf("fallback overwrote existing file: %v", err)
			}
			got, _ = os.ReadFile(destination)
			if !bytes.Equal(got, data) {
				t.Fatal("existing backup was overwritten")
			}
		})
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "failed-copy")
	if err := copyExclusiveFile(dir, destination); err == nil {
		t.Fatal("copy from directory succeeded")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("failed copy left a partial output")
	}
}
