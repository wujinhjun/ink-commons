//go:build linux || darwin

package ink

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
	_ "modernc.org/sqlite"
)

const stateDatabase = "state.sqlite3"
const schemaVersion = 6

// The lock filename is shared with deployed daemons and the macOS service manager.
const stateLock = "state.json.lock"

//go:embed sql/schema.sql
var stateSchema string

type stateStore struct{ db *sql.DB }

func openSQLite(path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state database must be a private regular file")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func openStore(path string) (*stateStore, error) {
	db, err := openSQLite(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("state database missing: check the data directory or restore a SQLite backup; for a new instance run ink state init")
	}
	if err != nil {
		return nil, err
	}
	if err := checkSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state database: %w", err)
	}
	return &stateStore{db: db}, nil
}

func checkSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != schemaVersion {
		return fmt.Errorf("unsupported state schema version %d (expected %d); check the data directory, restore a compatible SQLite backup, or initialize a separate directory with ink state init", version, schemaVersion)
	}
	// Fail before polling if a workflow table is missing from a damaged database.
	for _, table := range []string{"work_items", "work_tasks", "work_notifications", "pushes", "runtime_config", "workspace_allocations"} {
		rows, err := db.Query("SELECT * FROM " + table + " LIMIT 0")
		if err != nil {
			return fmt.Errorf("verify state table %s: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func checkStateDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("state directory missing: check the configured path or run ink state init for a new instance")
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("daemon state directory must be a directory with private permissions")
	}
	return nil
}

// InitializeState creates a new database on explicit request in an empty directory.
func InitializeState(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := checkStateDir(dir); err != nil {
		return err
	}
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != stateLock {
			return fmt.Errorf("initialization requires a fresh state directory: %s contains %s", dir, entry.Name())
		}
	}
	return createStateDatabase(filepath.Join(dir, stateDatabase))
}

func createStateDatabase(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ink-init-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Close(); err != nil {
		return err
	}
	if err := writeSchema(temp); err != nil {
		return err
	}
	return publishFile(temp, path)
}

func writeSchema(path string) error {
	db, err := openSQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(stateSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

type stateExport struct {
	Configurations []ConfigRecord        `json:"configurations,omitempty"`
	Workspaces     []WorkspaceAllocation `json:"workspaces,omitempty"`
	Pushes         []Push                `json:"pushes"`
	Work           map[string]*Work      `json:"work"`
}

func (s *stateStore) exportState() (stateExport, error) {
	st := stateExport{Work: make(map[string]*Work)}
	works, err := s.listWork()
	if err != nil {
		return st, err
	}
	for _, work := range works {
		st.Work[keyForWork(work)] = work
	}
	ctx := context.Background()
	pushes, err := statedb.New(s.db).ListPushes(ctx)
	if err != nil {
		return st, err
	}
	for _, row := range pushes {
		push, err := decodePush(row)
		if err != nil {
			return st, err
		}
		st.Pushes = append(st.Pushes, push)
	}
	st.Configurations, err = s.configRecords()
	if err != nil {
		return st, err
	}
	st.Workspaces, err = s.exportWorkspaces()
	return st, err
}

// Publish complete private files without replacing an existing destination.
func writeNewFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ink-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return publishFile(f.Name(), path)
}

func publishFile(temp, path string) error {
	return publishFileWithLink(temp, path, os.Link)
}

func publishFileWithLink(temp, path string, link func(string, string) error) error {
	if err := link(temp, path); err != nil {
		if !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.ENOSYS) {
			return err
		}
		// Filesystems without hard links still get exclusive creation and fsync.
		// An interrupted copy leaves a destination that must be inspected before retrying.
		if err := copyExclusiveFile(temp, path); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

func copyExclusiveFile(source, destination string) (err error) {
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		output.Close()
		if err != nil {
			os.Remove(destination)
		}
	}()
	if _, err = io.Copy(output, sourceFile); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	return output.Close()
}

// StateSnapshot requires the daemon to be stopped. Outputs never overwrite.
func StateSnapshot(dir, output string, asJSON bool) error {
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(dir, stateDatabase)
	s, err := openStore(path)
	if err != nil {
		return err
	}
	defer s.db.Close()
	if asJSON {
		st, err := s.exportState()
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		return writeNewFile(output, append(data, '\n'))
	}
	f, err := os.CreateTemp(filepath.Dir(output), ".ink-backup-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Close(); err != nil {
		return err
	}
	// VACUUM INTO produces a self-contained consistent SQLite snapshot.
	if _, err := s.db.Exec("VACUUM INTO ?", temp); err != nil {
		return err
	}
	f, err = os.OpenFile(temp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return publishFile(temp, output)
}

func lockState(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, stateLock), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock daemon state (stop ink serve first): %w", err)
	}
	return f, nil
}
