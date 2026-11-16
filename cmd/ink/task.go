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
	"time"

	"github.com/google/uuid"
	"ink-commons/internal/execution"
	"ink-commons/internal/ink"
)

type taskCredential struct {
	TaskID string           `json:"task_id"`
	Token  string           `json:"token"`
	Route  *execution.Route `json:"route,omitempty"`
	Login  string           `json:"github_login,omitempty"`
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
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if _, err := uuid.Parse(*taskID); err != nil || flags.NArg() != 0 {
		return errors.New("--task must be a task UUID, with no positional arguments")
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	path := "http://ink/v1/tasks/" + *taskID
	method := http.MethodGet
	var body io.Reader
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
		credential, err := readTaskCredential(file)
		if errors.Is(err, os.ErrNotExist) && action == "claim" {
			route := routeFromEnv(os.Getenv)
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
			if credential.Route != nil && (current.Provider != credential.Route.Provider || current.Target != credential.Route.Target || current.Session != credential.Route.Session) {
				return errors.New("claim file belongs to another agent route")
			}
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
		return fmt.Errorf("daemon HTTP %d: %s", res.StatusCode, problem.Error)
	}
	_, err = os.Stdout.Write(data)
	return err
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
