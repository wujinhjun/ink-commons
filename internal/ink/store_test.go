package ink

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"ink-commons/internal/execution"
)

func storedFixture() state {
	route := &execution.Route{Provider: "cc-connect", Target: "dev", Session: "discord:123", Parent: "456"}
	w := &Work{Repo: "acme/project", Linked: true, Origin: route, Developer: Assignee{GitHubLogin: "developer", Route: route}, Reviewer: Assignee{GitHubLogin: "reviewer", Route: &execution.Route{Provider: "http", Target: "reviewer", Session: "review/42"}}, Phase: "review_pending", Task: &Task{Role: "reviewer", SHA: strings.Repeat("a", 40), Delivery: "unknown", Error: "delivery interrupted or in progress", Attempts: 3}}
	w.Pull.Number, w.Pull.State, w.Pull.Head.SHA = 42, "open", strings.Repeat("a", 40)
	w.Pull.Head.Ref = "feature"
	w.Reviews = []Review{{ID: 123, SHA: strings.Repeat("b", 40), State: "CHANGES_REQUESTED", URL: "https://example.invalid/review", SubmittedAt: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}}
	accepted := *w
	accepted.Pull.Number = 43
	task := *w.Task
	task.Delivery, task.Error = "accepted", ""
	accepted.Task = &task
	return state{Pushes: []Push{{Repo: w.Repo, Ref: w.Pull.Head.Ref, SHA: w.Pull.Head.SHA, Route: *route, At: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}}, Work: map[string]*Work{"acme/project#42": w, "acme/project#43": &accepted}}
}

func checkState(t *testing.T, store *stateStore, want state) {
	t.Helper()
	got, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state changed:\ngot %+v\nwant %+v", got, want)
	}
}

func TestSQLiteMigrationRestartAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateDatabase)
	legacy := filepath.Join(dir, legacyState)
	initial := storedFixture()
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	checkState(t, store, initial)
	for _, p := range []string{legacy, legacy + ".pre-sqlite.bak"} {
		actual, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatalf("legacy backup %s: %s, %v", p, actual, err)
		}
	}
	// Keep migration-era data distinct so export and backup cannot accidentally
	// return the old JSON, and a restart must not import it again.
	initial.Work["acme/project#42"].Task.Attempts = 7
	initial.Work["acme/project#42"].Reviewer.Route.Session = "new-review-session"
	if err := store.save(initial); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("invalid obsolete JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	checkState(t, store, initial)
	store.db.Close()

	snapshot := filepath.Join(dir, "backup.sqlite3")
	exported := filepath.Join(dir, "rollback.json")
	if err := StateSnapshot(dir, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if err := StateSnapshot(dir, exported, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, snapshot, exported, legacy + ".pre-sqlite.bak"} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v, %v", p, info, err)
		}
	}
	if err := StateSnapshot(dir, exported, true); err == nil {
		t.Fatal("export overwrote existing file")
	}
	if err := StateSnapshot(dir, snapshot, false); err == nil {
		t.Fatal("backup overwrote existing file")
	}
	backup, err := openStore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	checkState(t, backup, initial)
	backup.db.Close()
	// Restore the SQLite backup into a new state directory.
	restoredDir := t.TempDir()
	snapshotData, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restoredDir, stateDatabase), snapshotData, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := openStore(filepath.Join(restoredDir, stateDatabase))
	if err != nil {
		t.Fatal(err)
	}
	checkState(t, restored, initial)
	restored.db.Close()
	// JSON export remains readable by the old state representation and can be
	// migrated again after running an older binary.
	exportedData, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	var oldState state
	if err := json.Unmarshal(exportedData, &oldState); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldState, initial) {
		t.Fatal("rollback lost current data")
	}
	newDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(newDir, legacyState), exportedData, 0600); err != nil {
		t.Fatal(err)
	}
	remigrated, err := openStore(filepath.Join(newDir, stateDatabase))
	if err != nil {
		t.Fatal(err)
	}
	defer remigrated.db.Close()
	checkState(t, remigrated, initial)
}

func TestMigrationFailureCanRetryWithoutPartialImport(t *testing.T) {
	for _, input := range []string{`{`, `{"work":null}`, `{"work":{"acme/project#42":null}}`, `{"work":{},"future":true}`} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, legacyState)
			path := filepath.Join(dir, stateDatabase)
			if err := os.WriteFile(legacy, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := openStore(path); err == nil {
				store.db.Close()
				t.Fatal("invalid state migrated")
			}
			got, err := os.ReadFile(legacy)
			if err != nil || string(got) != input {
				t.Fatal("failed migration changed legacy state")
			}
			if _, err := os.Stat(legacy + ".pre-sqlite.bak"); !os.IsNotExist(err) {
				t.Fatal("invalid state backed up")
			}
			fixture := storedFixture()
			data, _ := json.Marshal(fixture)
			if err := os.WriteFile(legacy, data, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.db.Close()
			checkState(t, store, fixture)
		})
	}
}

func TestSQLiteSaveRollsBackAndRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateDatabase)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	initial := storedFixture()
	if err := store.save(initial); err != nil {
		t.Fatal(err)
	}
	// Fail only after replacement has deleted the old rows. The transaction must
	// restore them all, including previously accepted/unknown delivery records.
	if _, err := store.db.Exec(`CREATE TRIGGER fail_save BEFORE INSERT ON pushes BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`); err != nil {
		t.Fatal(err)
	}
	replacement := storedFixture()
	replacement.Work["acme/project#42"].Task.Delivery = "pending"
	if err := store.save(replacement); err == nil {
		t.Fatal("expected failed write")
	}
	checkState(t, store, initial)
	if _, err := store.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	if reopened, err := openStore(path); err == nil {
		reopened.db.Close()
		t.Fatal("opened unsupported schema")
	}
}

func TestMigrationBackupConflictAndLock(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, legacyState)
	data, _ := json.Marshal(storedFixture())
	if err := os.WriteFile(legacy, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy+".pre-sqlite.bak", []byte("older backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if store, err := openStore(filepath.Join(dir, stateDatabase)); err == nil {
		store.db.Close()
		t.Fatal("overwrote migration backup")
	}
	if err := os.Remove(legacy + ".pre-sqlite.bak"); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(filepath.Join(dir, stateDatabase))
	if err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	lock, err := lockState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if second, err := lockState(legacy); err == nil {
		second.Close()
		t.Fatal("second daemon acquired legacy lock")
	}
	output := filepath.Join(dir, "backup.sqlite3")
	if err := StateSnapshot(dir, output, false); err == nil {
		t.Fatal("backup raced a running daemon")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed backup left an output")
	}
}

func TestSQLiteRejectsUnversionedAndInsecureFiles(t *testing.T) {
	for _, kind := range []string{"unversioned", "missing-table", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), stateDatabase)
			store, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unversioned":
				_, err = store.db.Exec("PRAGMA user_version=0")
			case "missing-table":
				_, err = store.db.Exec("DROP TABLE pushes")
			}
			if err != nil {
				t.Fatal(err)
			}
			store.db.Close()
			if kind == "public" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			}
			if reopened, err := openStore(path); err == nil {
				reopened.db.Close()
				t.Fatal("invalid database accepted")
			}
		})
	}
}

func TestSnapshotDoesNotCreateMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	for _, asJSON := range []bool{false, true} {
		output := filepath.Join(dir, "snapshot")
		if err := StateSnapshot(dir, output, asJSON); err == nil {
			t.Fatal("backed up missing database")
		}
		for _, path := range []string{output, filepath.Join(dir, stateDatabase)} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("created %s", path)
			}
		}
	}
}

func TestMissingMigratedDatabaseDoesNotReplayLegacyTasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateDatabase)
	initial := storedFixture()
	initial.Work["acme/project#42"].Task.Delivery = "pending"
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(filepath.Join(dir, legacyState), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	initial.Work["acme/project#42"].Task.Delivery = "accepted"
	initial.Work["acme/project#42"].Task.Attempts++
	if err := store.save(initial); err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	backup := filepath.Join(dir, "current.sqlite3")
	if err := StateSnapshot(dir, backup, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openStore(path); err == nil {
		reopened.db.Close()
		t.Fatal("silently imported stale pending task")
	} else if !strings.Contains(err.Error(), "database is missing") {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed startup created a new database")
	}
	restoredData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, restoredData, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.db.Close()
	checkState(t, restored, initial)
}

func TestInterruptedMigrationWithMatchingBackupCanRetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateDatabase)
	fixture := storedFixture()
	data, _ := json.Marshal(fixture)
	for name, content := range map[string][]byte{stateDatabase: nil, legacyState: data, legacyState + ".pre-sqlite.bak": data} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	checkState(t, store, fixture)
}

func TestMigrationMapsLegacyReviewThreadWithoutReplayingDelivery(t *testing.T) {
	for _, session := range []string{"", "discord:99", "discord:99:456", "discord:100"} {
		t.Run(session, func(t *testing.T) {
			dir := t.TempDir()
			data := []byte(fmt.Sprintf(`{"work":{"acme/project#42":{"repo":"acme/project","pull_request":{"number":42,"state":"open"},"linked":true,"origin":{"project":"dev","session_key":"discord:123"},"developer":{"route":{"project":"dev","session_key":"discord:123"}},"reviewer":{"github_login":"reviewer","route":{"project":"reviewer","session_key":%q}},"review_thread_id":"99","phase":"review_pending","task":{"role":"reviewer","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","delivery":"accepted","attempts":3}}}}`, session))
			if err := os.WriteFile(filepath.Join(dir, legacyState), data, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := openStore(filepath.Join(dir, stateDatabase))
			if session == "discord:100" {
				if err == nil {
					store.db.Close()
					t.Fatal("conflicting reviewer session migrated")
				}
				if !strings.Contains(err.Error(), "conflicts") {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.db.Close()
			st, err := store.load()
			if err != nil {
				t.Fatal(err)
			}
			work := st.Work["acme/project#42"]
			wantSession := session
			if wantSession == "" {
				wantSession = "discord:99"
			}
			if work.Reviewer.Route.Provider != "cc-connect" || work.Reviewer.Route.Target != "reviewer" || work.Reviewer.Route.Session != wantSession || work.Origin.Session != "discord:123" || work.Task.Delivery != "accepted" || work.Task.Attempts != 3 {
				t.Fatalf("legacy binding/delivery changed: %+v", work)
			}
			backup, err := os.ReadFile(filepath.Join(dir, legacyState+".pre-sqlite.bak"))
			if err != nil || !bytes.Equal(backup, data) {
				t.Fatal("legacy source not preserved byte-for-byte")
			}
		})
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

func TestInitializedStateRejectsEmptyDatabasePlaceholders(t *testing.T) {
	for _, kind := range []string{"zero-byte", "empty-sqlite"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, stateDatabase)
			fixture := storedFixture()
			fixture.Work["acme/project#42"].Task.Delivery = "pending"
			data, _ := json.Marshal(fixture)
			if err := os.WriteFile(filepath.Join(dir, legacyState), data, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Work["acme/project#42"].Task.Delivery = "accepted"
			if err := store.save(fixture); err != nil {
				t.Fatal(err)
			}
			store.db.Close()
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			marker, err := os.Stat(path + ".initialized")
			if err != nil || marker.Mode().Perm() != 0600 {
				t.Fatal("initialization marker missing or not private")
			}
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "empty-sqlite" {
				empty, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := empty.Exec("VACUUM"); err != nil {
					t.Fatal(err)
				}
				empty.Close()
			}
			if reopened, err := openStore(path); err == nil {
				reopened.db.Close()
				t.Fatal("stale pending task re-imported into empty database")
			}
			if err := os.WriteFile(path, saved, 0600); err != nil {
				t.Fatal(err)
			}
			restored, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.db.Close()
			checkState(t, restored, fixture)
		})
	}
}
