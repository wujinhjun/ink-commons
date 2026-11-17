package ink

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ink-commons/internal/ink/statedb"
	_ "modernc.org/sqlite"
)

const stateDatabase = "state.sqlite3"
const legacyState = "state.json"
const schemaVersion = 4

type state struct {
	// Configurations is populated only for JSON export/import. Workflow saves
	// deliberately ignore it; runtime policy has a separate transactional API.
	Configurations []ConfigRecord   `json:"configurations,omitempty"`
	Repositories   []Repository     `json:"repositories,omitempty"`
	Pushes         []Push           `json:"pushes"`
	Work           map[string]*Work `json:"work"`
}

// stateStore keeps the workflow snapshot API; each read and incremental save is a
// transaction. Service.mu and the daemon lock serialize read-modify-write cycles.
// Work payloads retain routes, reviews and delivery outcomes without re-deriving
// them on restart (in particular, unknown/accepted tasks must not be replayed).
type stateStore struct{ db *sql.DB }

func openStore(path string) (*stateStore, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	marker := path + ".initialized"
	markerInfo, markerErr := os.Lstat(marker)
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return nil, markerErr
	}
	if markerErr == nil && (!markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm()&0077 != 0) {
		return nil, errors.New("state initialization marker must be a private regular file")
	}
	// Neither a removed database nor a recovery tool's empty placeholder may
	// resurrect stale tasks after this state directory has been initialized.
	if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0 && markerErr == nil) {
		backup := filepath.Join(filepath.Dir(path), legacyState+".pre-sqlite.bak")
		_, backupErr := os.Lstat(backup)
		if markerErr == nil || (backupErr == nil && errors.Is(err, os.ErrNotExist)) {
			return nil, errors.New("state database is missing or empty after initialization; restore a SQLite backup; to reset state or intentionally import JSON, archive the initialization marker and migration backup first")
		}
		if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
			return nil, backupErr
		}
	} else if err != nil {
		return nil, err
	}
	// Pre-create privately: SQLite's default file mode depends on the process umask.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		var info os.FileInfo
		info, err = os.Lstat(path)
		if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
			err = errors.New("state database must be a private regular file")
		}
	}
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &stateStore{db: db}
	if err = s.initialize(filepath.Join(filepath.Dir(path), legacyState), marker); err != nil {
		db.Close()
		return nil, err
	}
	if markerErr != nil {
		if err := writeNewFile(marker, []byte("1\n")); err != nil {
			db.Close()
			return nil, fmt.Errorf("persist state initialization marker: %w", err)
		}
	}
	return s, nil
}

func (s *stateStore) initialize(legacy, marker string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		if _, err := tx.Exec(`CREATE TABLE repositories (name TEXT PRIMARY KEY, payload TEXT NOT NULL); PRAGMA user_version = 2;`); err != nil {
			return err
		}
		version = 2
	}
	// Version 3 fences older binaries that would discard cooperative claims.
	if version == 2 {
		if _, err := tx.Exec("PRAGMA user_version = 3"); err != nil {
			return err
		}
		version = 3
	}
	if version == 3 {
		if _, err := tx.Exec(`CREATE TABLE runtime_config (scope TEXT PRIMARY KEY, payload TEXT NOT NULL, revision INTEGER NOT NULL); PRAGMA user_version = 4;`); err != nil {
			return err
		}
		version = 4
	}
	if version == schemaVersion {
		// Detect missing tables before serving; never recreate damaged workflow tables.
		for _, table := range []string{"work_items", "pushes", "repositories", "runtime_config"} {
			rows, err := tx.Query("SELECT * FROM " + table + " LIMIT 0")
			if err != nil {
				return err
			}
			rows.Close()
		}
		return tx.Commit()
	}
	if version != 0 {
		return fmt.Errorf("unsupported state schema version %d (expected %d)", version, schemaVersion)
	}
	if _, err := os.Lstat(marker); err == nil {
		return errors.New("state database is uninitialized but an initialization marker exists; restore the database instead of importing stale JSON")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Only initialize an empty database. A foreign/unversioned database must not
	// be silently adopted or overwritten.
	var tables int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables != 0 {
		return errors.New("unversioned state database is not empty")
	}
	if _, err := tx.Exec(`CREATE TABLE work_items (id TEXT PRIMARY KEY, payload TEXT NOT NULL);
 CREATE TABLE pushes (position INTEGER PRIMARY KEY, payload TEXT NOT NULL);
 CREATE TABLE repositories (name TEXT PRIMARY KEY, payload TEXT NOT NULL);
 CREATE TABLE runtime_config (scope TEXT PRIMARY KEY, payload TEXT NOT NULL, revision INTEGER NOT NULL);`); err != nil {
		return err
	}
	data, err := os.ReadFile(legacy)
	if err == nil {
		st, err := decodeState(data)
		if err != nil {
			return fmt.Errorf("import %s: %w", legacy, err)
		}
		backup := legacy + ".pre-sqlite.bak"
		if previous, err := os.ReadFile(backup); err == nil {
			info, statErr := os.Lstat(backup)
			if statErr != nil {
				return statErr
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("legacy migration backup must be a private regular file")
			}
			if !bytes.Equal(previous, data) {
				return errors.New("legacy migration backup differs from state.json; preserve both before retrying")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := writeNewFile(backup, data); err != nil {
				return fmt.Errorf("back up legacy state: %w", err)
			}
		} else {
			return err
		}
		if err := syncState(tx, st); err != nil {
			return err
		}
		if err := importConfigRecords(tx, st.Configurations); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func validateState(st state) error {
	if st.Work == nil {
		return errors.New("state is missing its work index")
	}
	seen := map[string]bool{}
	for _, repo := range st.Repositories {
		if !ValidRepository(repo.Repo) || seen[repo.Repo] || strings.TrimSpace(repo.ReviewerLogin) == "" {
			return errors.New("invalid registered repository")
		}
		if err := repo.AutoMerge.Validate(); err != nil {
			return err
		}
		if err := repo.PostApproval.Validate(); err != nil {
			return err
		}
		if err := repo.ValidateReviewFeatures(); err != nil {
			return err
		}
		seen[repo.Repo] = true
	}
	for id, w := range st.Work {
		if w == nil || !ValidRepository(w.Repo) || w.Pull.Number <= 0 || id != key(w.Repo, w.Pull.Number) {
			return fmt.Errorf("invalid work index entry %q", id)
		}
	}
	return nil
}

func decodeState(data []byte) (state, error) {
	var raw struct {
		Configurations []ConfigRecord             `json:"configurations,omitempty"`
		Repositories   []Repository               `json:"repositories,omitempty"`
		Pushes         []Push                     `json:"pushes"`
		Work           map[string]json.RawMessage `json:"work"`
	}
	var st state
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil {
		return st, err
	}
	if d.Decode(new(any)) != io.EOF {
		return st, errors.New("expected one state object")
	}
	st.Configurations = raw.Configurations
	st.Repositories = raw.Repositories
	st.Pushes = raw.Pushes
	if raw.Work != nil {
		st.Work = make(map[string]*Work, len(raw.Work))
	}
	for id, payload := range raw.Work {
		entry := struct {
			*Work
			ReviewThread string `json:"review_thread_id"`
		}{Work: &Work{}}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return st, fmt.Errorf("work %s: %w", id, err)
		}
		if entry.ReviewThread != "" {
			if strings.Trim(entry.ReviewThread, "0123456789") != "" {
				return st, fmt.Errorf("work %s: invalid legacy review_thread_id", id)
			}
			route := entry.Reviewer.Route
			if route == nil || route.Provider != "cc-connect" || strings.TrimSpace(route.Target) == "" {
				return st, fmt.Errorf("work %s: legacy review_thread_id needs a cc-connect reviewer route", id)
			}
			session := "discord:" + entry.ReviewThread
			if route.Session != "" && route.Session != session {
				user, sameThread := strings.CutPrefix(route.Session, session+":")
				if !sameThread || user == "" || strings.Trim(user, "0123456789") != "" {
					return st, fmt.Errorf("work %s: review_thread_id conflicts with reviewer session", id)
				}
			}
			// Route.UnmarshalJSON already maps legacy project/session_key fields.
			copy := *route
			if copy.Session == "" {
				copy.Session = session
			}
			entry.Reviewer.Route = &copy
		}
		st.Work[id] = entry.Work
	}
	return st, validateState(st)
}

func (s *stateStore) load() (state, error) {
	st := state{Work: make(map[string]*Work)}
	tx, err := s.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	q := statedb.New(tx)
	ctx := context.Background()
	workRows, err := q.ListWork(ctx)
	if err != nil {
		return st, err
	}
	for _, row := range workRows {
		var work Work
		if err := json.Unmarshal([]byte(row.Payload), &work); err != nil {
			return st, err
		}
		st.Work[row.ID] = &work
	}
	pushRows, err := q.ListPushes(ctx)
	if err != nil {
		return st, err
	}
	for _, row := range pushRows {
		var push Push
		if err := json.Unmarshal([]byte(row.Payload), &push); err != nil {
			return st, err
		}
		st.Pushes = append(st.Pushes, push)
	}
	repoRows, err := q.ListRepositories(ctx)
	if err != nil {
		return st, err
	}
	for _, row := range repoRows {
		var repo Repository
		if err := json.Unmarshal([]byte(row.Payload), &repo); err != nil {
			return st, err
		}
		st.Repositories = append(st.Repositories, repo)
	}
	if err := validateState(st); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

// syncState compares persisted payloads in the same transaction, avoiding a
// mutable in-memory cache and preserving rollback/restart behavior. The snapshot
// API still requires Service.mu and the daemon lock around read-modify-write.
func syncState(tx *sql.Tx, st state) error {
	if err := validateState(st); err != nil {
		return err
	}
	ctx := context.Background()
	q := statedb.New(tx)
	workRows, err := q.ListWork(ctx)
	if err != nil {
		return err
	}
	repoRows, err := q.ListRepositories(ctx)
	if err != nil {
		return err
	}
	pushRows, err := q.ListPushes(ctx)
	if err != nil {
		return err
	}
	workPayloads := make(map[string]string, len(workRows))
	for _, row := range workRows {
		workPayloads[row.ID] = row.Payload
	}
	repoPayloads := make(map[string]string, len(repoRows))
	for _, row := range repoRows {
		repoPayloads[row.Name] = row.Payload
	}
	pushPayloads := make(map[int64]string, len(pushRows))
	for _, row := range pushRows {
		pushPayloads[row.Position] = row.Payload
	}
	for _, repo := range st.Repositories {
		data, err := json.Marshal(repo)
		if err != nil {
			return err
		}
		if old, exists := repoPayloads[repo.Repo]; !exists || old != string(data) {
			if err := q.PutRepository(ctx, statedb.PutRepositoryParams{Name: repo.Repo, Payload: string(data)}); err != nil {
				return err
			}
		}
		delete(repoPayloads, repo.Repo)
	}
	for id, work := range st.Work {
		data, err := json.Marshal(work)
		if err != nil {
			return err
		}
		if old, exists := workPayloads[id]; !exists || old != string(data) {
			if err := q.PutWork(ctx, statedb.PutWorkParams{ID: id, Payload: string(data)}); err != nil {
				return err
			}
		}
		delete(workPayloads, id)
	}
	for i, push := range st.Pushes {
		data, err := json.Marshal(push)
		if err != nil {
			return err
		}
		if old, exists := pushPayloads[int64(i)]; !exists || old != string(data) {
			if err := q.PutPush(ctx, statedb.PutPushParams{Position: int64(i), Payload: string(data)}); err != nil {
				return err
			}
		}
		delete(pushPayloads, int64(i))
	}
	for id := range workPayloads {
		if err := q.DeleteWork(ctx, id); err != nil {
			return err
		}
	}
	for name := range repoPayloads {
		if err := q.DeleteRepository(ctx, name); err != nil {
			return err
		}
	}
	for position := range pushPayloads {
		if err := q.DeletePush(ctx, position); err != nil {
			return err
		}
	}
	return nil
}

func (s *stateStore) save(st state) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := syncState(tx, st); err != nil {
		return err
	}
	return tx.Commit()
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
		// An interrupted copy may leave an incomplete destination; migration will
		// reject it on retry rather than accept a different/partial legacy backup.
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

// StateSnapshot requires the daemon to be stopped. It shares the legacy lock so
// neither old nor new daemons can race a backup/export. Outputs never overwrite.
// JSON export includes all current data, rather than the pre-migration snapshot.
func StateSnapshot(dir, output string, asJSON bool) error {
	lock, err := lockState(filepath.Join(dir, legacyState))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(dir, stateDatabase)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("state database: %w", err)
	}
	s, err := openStore(path)
	if err != nil {
		return err
	}
	defer s.db.Close()
	st, err := s.load()
	if err != nil {
		return err
	}
	st.Configurations, err = s.configRecords()
	if err != nil {
		return err
	}
	if asJSON {
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

func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock daemon state (stop ink serve first): %w", err)
	}
	return f, nil
}
