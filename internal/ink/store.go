package ink

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

const stateDatabase = "state.sqlite3"
const legacyState = "state.json"
const schemaVersion = 1

type state struct {
	Pushes []Push           `json:"pushes"`
	Work   map[string]*Work `json:"work"`
}

// stateStore keeps the workflow snapshot API; each read and replacement is a
// transaction. Service.mu and the daemon lock serialize read-modify-write cycles.
// Work payloads retain routes, reviews and delivery outcomes without re-deriving
// them on restart (in particular, unknown/accepted tasks must not be replayed).
type stateStore struct{ db *sql.DB }

func openStore(path string) (*stateStore, error) {
	path, err := filepath.Abs(path)
	if err != nil {
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
	if err = s.initialize(filepath.Join(filepath.Dir(path), legacyState)); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *stateStore) initialize(legacy string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		// Detect missing tables before serving; never recreate a damaged v1 store.
		for _, table := range []string{"work_items", "pushes"} {
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
 CREATE TABLE pushes (position INTEGER PRIMARY KEY, payload TEXT NOT NULL);`); err != nil {
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
		if err := replaceState(tx, st); err != nil {
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
	for id, w := range st.Work {
		if w == nil || !ValidRepository(w.Repo) || w.Pull.Number <= 0 || id != key(w.Repo, w.Pull.Number) {
			return fmt.Errorf("invalid work index entry %q", id)
		}
	}
	return nil
}

func decodeState(data []byte) (state, error) {
	var st state
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&st); err != nil {
		return st, err
	}
	if d.Decode(new(any)) != io.EOF {
		return st, errors.New("expected one state object")
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
	rows, err := tx.Query("SELECT id, payload FROM work_items ORDER BY id")
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id, payload string
		var work Work
		if err = rows.Scan(&id, &payload); err == nil {
			err = json.Unmarshal([]byte(payload), &work)
		}
		if err != nil {
			rows.Close()
			return st, err
		}
		st.Work[id] = &work
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return st, err
	}
	rows, err = tx.Query("SELECT payload FROM pushes ORDER BY position")
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var payload string
		var push Push
		if err = rows.Scan(&payload); err == nil {
			err = json.Unmarshal([]byte(payload), &push)
		}
		if err != nil {
			rows.Close()
			return st, err
		}
		st.Pushes = append(st.Pushes, push)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return st, err
	}
	if err := validateState(st); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

func replaceState(tx *sql.Tx, st state) error {
	if err := validateState(st); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM work_items; DELETE FROM pushes;"); err != nil {
		return err
	}
	for id, work := range st.Work {
		data, err := json.Marshal(work)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO work_items(id, payload) VALUES (?, ?)", id, string(data)); err != nil {
			return err
		}
	}
	for i, push := range st.Pushes {
		data, err := json.Marshal(push)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO pushes(position, payload) VALUES (?, ?)", i, string(data)); err != nil {
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
	if err := replaceState(tx, st); err != nil {
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
	if err := os.Link(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
