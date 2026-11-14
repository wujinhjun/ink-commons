package ink

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type state struct {
	Pushes []Push           `json:"pushes"`
	Work   map[string]*Work `json:"work"`
}

func openState(path string) (state, error) {
	s := state{Work: make(map[string]*Work)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read state: %w", err)
	}
	if s.Work == nil {
		return s, errors.New("state is missing its work index")
	}
	return s, nil
}

func saveState(path string, s state) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ink-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(s); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock daemon state: %w", err)
	}
	return f, nil
}
