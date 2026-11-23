package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"ink-commons/internal/execution"
	"ink-commons/internal/ink"
)

type taskCredential struct {
	TaskID    string           `json:"task_id"`
	Token     string           `json:"token"`
	Route     *execution.Route `json:"route,omitempty"`
	Login     string           `json:"github_login,omitempty"`
	Rejected  bool             `json:"rejected,omitempty"`
	AttemptID string           `json:"attempt_id,omitempty"`
}

func taskCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ink task claim|status|submit|retry --task TASK_ID [flags]")
	}
	action := args[0]
	if action != "claim" && action != "status" && action != "submit" && action != "retry" {
		return errors.New("expected task claim, status, submit or retry")
	}
	flags := flag.NewFlagSet("task "+action, flag.ContinueOnError)
	taskID := flags.String("task", "", "task ID from the wake or work API")
	claimFile := flags.String("claim-file", "", "private credential file (default: state directory/claims/TASK.json)")
	login := flags.String("github-login", "", "assigned human login when the task has no agent route")
	requestID := flags.String("request-id", "", "stable idempotency key for this submission")
	handoff := flags.Bool("handoff", false, "confirm all task tools/writes have finished and relinquish this task")
	newSHA := flags.String("new-sha", "", "pushed developer result SHA")
	reviewID := flags.Int64("review-id", 0, "formal GitHub review ID")
	outcome := flags.String("outcome", "", "APPROVED, CHANGES_REQUESTED or blocked")
	reason := flags.String("reason", "", "reason for a blocked handoff")
	routeProvider := flags.String("provider", "", "explicit executor provider for the assigned task route")
	routeTarget := flags.String("target", "", "explicit executor target/project for the assigned task route")
	routeSession := flags.String("session", "", "explicit executor session for the assigned task route")
	replaceRoute := flags.Bool("replace-route", false, "replace a saved route after a confirmed rejected claim; requires --provider/--target/--session")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if _, err := uuid.Parse(*taskID); err != nil || flags.NArg() != 0 {
		return errors.New("--task must be a task UUID, with no positional arguments")
	}
	explicitRoute, err := explicitTaskRoute(*routeProvider, *routeTarget, *routeSession)
	if err != nil {
		return err
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	path := "http://ink/v1/tasks/" + *taskID
	method := http.MethodGet
	var body io.Reader
	credentialPath := ""
	claimToken := ""
	claimAttemptID := ""
	if action != "status" {
		file := *claimFile
		if file == "" {
			folder := filepath.Join(dir, "claims")
			if err := os.MkdirAll(folder, 0700); err != nil {
				return err
			}
			info, err := os.Lstat(folder)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
				return errors.New("claims directory must be a private real directory")
			}
			file = filepath.Join(folder, *taskID+".json")
		}
		lock, err := lockTaskCredential(file)
		if err != nil {
			return err
		}
		defer unlockTaskCredential(lock)
		credentialPath = file
		credential, err := readTaskCredential(file)
		if errors.Is(err, os.ErrNotExist) && action == "claim" {
			route := routeFromEnv(os.Getenv)
			if explicitRoute != nil {
				route = *explicitRoute
			}
			credential = taskCredential{TaskID: *taskID, Login: *login}
			if route.Valid(false) {
				credential.Route = &route
			} else if *login == "" {
				return errors.New("claim requires the assigned agent route environment or --github-login for a human task")
			}
			token := make([]byte, 32)
			if _, err := rand.Read(token); err != nil {
				return err
			}
			credential.Token = hex.EncodeToString(token)
			data, _ := json.Marshal(credential)
			// Persist before sending so an ambiguous response can be recovered without
			// minting another identity. O_EXCL prevents competing wakes replacing it.
			if err := writeTaskCredential(file, append(data, '\n')); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return err
				}
				credential, err = readTaskCredential(file)
				if err != nil {
					return err
				}
			}
		} else if err != nil {
			return fmt.Errorf("read private claim credential: %w", err)
		}
		if credential.TaskID != *taskID || len(credential.Token) < 32 {
			return errors.New("claim file does not belong to this task")
		}
		if action == "claim" {
			current := routeFromEnv(os.Getenv)
			if explicitRoute != nil {
				current = *explicitRoute
			}
			switch {
			case *replaceRoute:
				if explicitRoute == nil {
					return errors.New("--replace-route requires --provider, --target and --session")
				}
				credential.Route = explicitRoute
				credential.Login = ""
				credential.Rejected = false
			case credential.Rejected:
				switch {
				case explicitRoute != nil:
					credential.Route = explicitRoute
					credential.Login = ""
				case current.Valid(false):
					credential.Route = &current
					credential.Login = ""
				case *login != "":
					credential.Route = nil
					credential.Login = *login
				default:
					return errors.New("previous claim was rejected; supply the assigned route with --provider/--target/--session or --github-login")
				}
				credential.Rejected = false
			case credential.Route != nil && (current.Provider != credential.Route.Provider || current.Target != credential.Route.Target || current.Session != credential.Route.Session):
				return errors.New("claim file belongs to another agent route; after a confirmed 403 retry with --replace-route and the assigned route")
			}
			attemptID, err := newAttemptID()
			if err != nil {
				return err
			}
			credential.AttemptID = attemptID
			if err := replaceTaskCredential(file, credential); err != nil {
				return fmt.Errorf("record claim attempt: %w", err)
			}
			claimToken = credential.Token
			claimAttemptID = credential.AttemptID
		}
		input := struct {
			Token      string                 `json:"token"`
			Route      *execution.Route       `json:"route,omitempty"`
			Login      string                 `json:"github_login,omitempty"`
			Submission *ink.HandoffSubmission `json:"submission,omitempty"`
		}{Token: credential.Token, Route: credential.Route, Login: credential.Login}
		if action == "submit" {
			input.Submission = &ink.HandoffSubmission{RequestID: *requestID, Handoff: *handoff, NewSHA: *newSHA, ReviewID: *reviewID, Outcome: *outcome, Reason: *reason}
		}
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
		method = http.MethodPost
		path += "/" + action
	}
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := localClient(dir, 10*time.Second).Do(req)
	if err != nil {
		return errors.New("daemon unavailable; keep the claim file and retry the same request, do not repeat code work")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("task response too large")
	}
	if res.StatusCode != 200 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &problem)
		if action == "claim" && res.StatusCode == http.StatusForbidden {
			if credentialPath != "" {
				if err := markTaskCredentialRejected(credentialPath, *taskID, claimToken, claimAttemptID); err != nil {
					return fmt.Errorf("daemon HTTP %d: %s; failed to record rejected claim: %w", res.StatusCode, problem.Error, err)
				}
			}
			return fmt.Errorf("daemon HTTP %d: %s; the claim was rejected and can be retried with the assigned route or --github-login", res.StatusCode, problem.Error)
		}
		return fmt.Errorf("daemon HTTP %d: %s", res.StatusCode, problem.Error)
	}
	_, err = os.Stdout.Write(data)
	return err
}

// explicitTaskRoute lets runtimes that do not inject CC_PROJECT/CC_SESSION_KEY
// bind a claim to the route carried by the wake prompt.
func explicitTaskRoute(provider, target, session string) (*execution.Route, error) {
	if strings.TrimSpace(provider) == "" && strings.TrimSpace(target) == "" && strings.TrimSpace(session) == "" {
		return nil, nil
	}
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(target) == "" || strings.TrimSpace(session) == "" {
		return nil, errors.New("--provider, --target and --session must be provided together")
	}
	route := execution.Route{Provider: provider, Target: target, Session: session}
	if !route.Valid(false) {
		return nil, errors.New("explicit task route is invalid")
	}
	return &route, nil
}

func markTaskCredentialRejected(path, taskID, token, attemptID string) error {
	credential, err := readTaskCredential(path)
	if err != nil {
		return err
	}
	if credential.TaskID != taskID || len(credential.Token) < 32 {
		return errors.New("claim file does not belong to this task")
	}
	if token == "" || credential.Token != token || attemptID == "" || credential.AttemptID != attemptID {
		// A delayed rejection belongs to an older attempt; a newer route may
		// already own this credential. Never let it overwrite the newer state.
		return nil
	}
	credential.Rejected = true
	return replaceTaskCredential(path, credential)
}

func newAttemptID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func lockTaskCredential(path string) (*os.File, error) {
	// O_NOFOLLOW prevents a custom --claim-file sidecar from redirecting the
	// lock to an unrelated file. Never chmod an existing lock: only accept a
	// private regular file that was created with the expected mode.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, errors.New("claim lock must be a private regular file")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			lock.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			lock.Close()
			return nil, errors.New("claim credential is locked by another attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unlockTaskCredential(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func replaceTaskCredential(path string, credential taskCredential) error {
	data, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readTaskCredential(path string) (taskCredential, error) {
	var c taskCredential
	info, err := os.Lstat(path)
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return c, errors.New("claim credential must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

func writeTaskCredential(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
