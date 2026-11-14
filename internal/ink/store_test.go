package ink

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
