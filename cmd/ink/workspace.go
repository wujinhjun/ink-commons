package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"ink-commons/internal/ink"
	"ink-commons/internal/workspace"
)

func workspaceCommand(args []string) error {
	usage := errors.New("usage: ink workspace list|acquire|release|remove [flags]")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list", "acquire", "release", "remove":
	default:
		return usage
	}
	flags := flag.NewFlagSet("workspace "+args[0], flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "emit JSON")
	repo, kind, ref, branch, request, id, task, claimFile := ".", "auto", "", "", "", "", "", ""
	switch args[0] {
	case "list":
		flags.StringVar(&repo, "repo", repo, "local clone or worktree")
	case "acquire":
		flags.StringVar(&repo, "repo", repo, "local clone or worktree")
		flags.StringVar(&kind, "kind", kind, "auto, full, or lite")
		flags.StringVar(&ref, "ref", ref, "local starting commit (defaults to branch or HEAD)")
		flags.StringVar(&branch, "branch", branch, "existing or new branch (omit for detached HEAD)")
		flags.StringVar(&request, "request-id", request, "stable ID for this allocation request")
		flags.StringVar(&task, "task", task, "associate a currently claimed task (optional)")
		flags.StringVar(&claimFile, "claim-file", claimFile, "private task claim file (optional)")
	default:
		flags.StringVar(&id, "id", id, "workspace allocation ID")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected workspace arguments")
	}
	repo, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return listWorkspaces(repo, *asJSON)
	case "acquire":
		if request == "" {
			return errors.New("workspace acquire requires --request-id")
		}
		payload := map[string]any{"source": repo, "request_id": request, "preference": kind, "ref": ref, "branch": branch, "task_id": task}
		if task != "" {
			credential, err := workspaceTaskCredential(task, claimFile)
			if err != nil {
				return err
			}
			payload["owner"], payload["claim_token"] = credential.Route, credential.Token
		} else if claimFile != "" {
			return errors.New("--claim-file requires --task")
		} else if route := routeFromEnv(os.Getenv); route.Valid(false) {
			payload["owner"] = route
		}
		return changeWorkspace("/v1/workspaces", payload, *asJSON)
	default:
		if id == "" {
			return fmt.Errorf("workspace %s requires --id", args[0])
		}
		return changeWorkspace("/v1/workspaces/"+url.PathEscape(id)+"/"+args[0], nil, *asJSON)
	}
}

func workspaceTaskCredential(task, path string) (taskCredential, error) {
	if _, err := uuid.Parse(task); err != nil {
		return taskCredential{}, errors.New("--task must be a task UUID")
	}
	if path == "" {
		dir, err := directory()
		if err != nil {
			return taskCredential{}, err
		}
		path = filepath.Join(dir, "claims", task+".json")
	}
	credential, err := readTaskCredential(path)
	if err != nil {
		return taskCredential{}, fmt.Errorf("read private claim credential: %w", err)
	}
	if credential.TaskID != task || credential.Rejected || credential.Route == nil {
		return taskCredential{}, errors.New("workspace requires a valid routed claim for this task")
	}
	return credential, nil
}

type workspaceInventory struct {
	workspace.Repository
	Allocations []ink.WorkspaceAllocation `json:"allocations,omitempty"`
}

func listWorkspaces(repo string, asJSON bool) error {
	var inventory workspaceInventory
	found, err := workspaceAPI(http.MethodGet, "/v1/workspaces?repo="+url.QueryEscape(repo), nil, &inventory)
	if err != nil {
		return err
	}
	if !found {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		inventory.Repository, err = workspace.Inspect(ctx, repo)
		if err != nil {
			return err
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(inventory)
	}
	managed := make(map[string]ink.WorkspaceAllocation)
	for _, allocation := range inventory.Allocations {
		managed[allocation.Path] = allocation
	}
	for _, entry := range inventory.Worktree {
		fmt.Printf("%s\t%s\t%s", entry.Path, entry.Branch, entry.Head)
		if a, ok := managed[entry.Path]; ok {
			fmt.Printf("\t%s\t%s\t%s", a.Kind, a.State, a.ID)
		}
		switch {
		case entry.Prunable:
			fmt.Print("\tprunable")
		case entry.Error != "":
			fmt.Printf("\tunknown (%s)", entry.Error)
		case entry.Dirty:
			fmt.Print("\tdirty")
		}
		if entry.Locked {
			fmt.Print("\tlocked")
		}
		fmt.Println()
		delete(managed, entry.Path)
	}
	for _, a := range inventory.Allocations {
		if _, ok := managed[a.Path]; ok {
			fmt.Printf("%s\t%s\t%s\t%s\tmissing\n", a.Path, a.Kind, a.State, a.ID)
		}
	}
	return nil
}

func changeWorkspace(path string, payload any, asJSON bool) error {
	var result ink.WorkspaceAllocation
	found, err := workspaceAPI(http.MethodPost, path, payload, &result)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("workspace operations require a running ink daemon with workspace support")
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", result.ID, result.Kind, result.State, result.Path)
	return nil
}

func workspaceAPI(method, path string, payload any, result any) (bool, error) {
	dir, err := directory()
	if err != nil {
		return false, err
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://ink"+path, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := localClient(dir, 29*time.Second).Do(req)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return false, nil
		}
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// A privileged daemon disables this endpoint. Keep read-only inventory
		// available via the caller's local Git without bypassing write denials.
		if method == http.MethodGet && res.StatusCode == http.StatusForbidden {
			return false, nil
		}
		var problem struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&problem)
		if res.StatusCode == http.StatusNotFound && problem.Error == "" {
			return false, nil
		}
		return true, fmt.Errorf("workspace HTTP %d: %s", res.StatusCode, problem.Error)
	}
	return true, json.NewDecoder(res.Body).Decode(result)
}
