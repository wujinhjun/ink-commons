//go:build linux || darwin

package ink

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func safeCredentialCommand(path string) bool {
	if !filepath.IsAbs(path) || filepath.Base(path) == "gh" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err != nil || filepath.Base(resolved) != "gh"
}

func isolateCredentialCommand(cmd *exec.Cmd, secretEnv []string) (func(), error) {
	dir, err := os.MkdirTemp("", "ink-github-")
	if err != nil {
		return nil, diagnosticError("cannot isolate GitHub command environment")
	}
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
			"INK_GITHUB_TOKEN", "INK_GITHUB_ROLE":
			return true
		}
		return slices.Contains(secretEnv, name) || strings.HasPrefix(name, "GIT_") ||
			strings.HasPrefix(name, "GH_")
	})
	cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+dir, "GH_PROMPT_DISABLED=1", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	return func() { _ = os.RemoveAll(dir) }, nil
}

func credentialCommandReference(command string, args []string) (string, error) {
	if !safeCredentialCommand(command) {
		return "", diagnosticError("GitHub credential command requires an explicit wrapper")
	}
	if _, err := exec.LookPath(command); err != nil {
		return "", diagnosticError("GitHub credential command is unavailable")
	}
	file, err := os.Open(command)
	if err != nil {
		return "", diagnosticError("GitHub credential command is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", diagnosticError("invalid GitHub credential command")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", diagnosticError("GitHub credential command cannot be read")
	}
	return fingerprint(append([]string{command, fmt.Sprintf("%x", digest.Sum(nil))}, args...)...), nil
}

func (u upstream) apiHost() string {
	api, err := url.Parse(u.github)
	if err != nil || api.Host == "" {
		return ""
	}
	if u.github == "https://api.github.com" {
		return "github.com"
	}
	return strings.ToLower(api.Host)
}

func (u upstream) source(c GitHubCredential, literal string) tokenSource {
	return tokenSource{config: c, literal: literal, getenv: os.Getenv, client: u.http,
		api: u.github, now: time.Now, cache: u.tokens, throttle: u.throttle, secretEnv: u.secretEnv}
}

func (u upstream) credentialReference(role githubRole) (string, error) {
	if u.command != "" {
		if err := checkCommandDigest(u.command, u.commandDigest); err != nil {
			return "", diagnosticError(err.Error())
		}
		args := u.readArgs
		if role == githubMerger {
			args = u.mergeArgs
		}
		ref, err := credentialCommandReference(u.command, args)
		if err != nil {
			return "", err
		}
		return fingerprint("legacy", ref, fmt.Sprint(args == nil)), nil
	}
	if u.credential != nil {
		return u.source(*u.credential, "").reference()
	}
	if u.token != "" {
		return u.source(GitHubCredential{}, u.token).reference()
	}
	return "", nil
}

func (u upstream) mergeIdentity(repo string) (string, error) {
	ref, err := u.credentialReference(githubMerger)
	if err != nil {
		return "", err
	}
	if ref == "" {
		return "", diagnosticError("automatic merge requires a dedicated GitHub merger credential")
	}
	return scopedMergeIdentity(u.apiHost(), repo, ref), nil
}

func (u upstream) requestToken(ctx context.Context, repo string, role githubRole) (string, error) {
	if u.credential == nil && u.token == "" {
		return "", nil
	}
	ref, err := u.credentialReference(role)
	if err != nil {
		return "", err
	}
	if err := u.compareMergeIdentity(repo, ref); err != nil {
		return "", err
	}
	src := GitHubCredential{}
	if u.credential != nil {
		src = *u.credential
	}
	var provider TokenProvider = u.source(src, u.token)
	token, _, err := provider.Token(ctx, tokenScope{u.apiHost(), strings.ToLower(repo), ref, role})
	return token, err
}

func (u upstream) mergeRole(reuseReader bool) (upstream, error) {
	switch {
	case reuseReader && (u.command != "" || u.credential != nil && u.credential.App != nil):
		return u, nil
	case reuseReader:
		return upstream{}, diagnosticError("auto_merge.reuse_reader_identity requires a GitHub App")
	case u.mergerCommand != "":
		u.command, u.commandDigest, u.credential, u.token = u.mergerCommand, u.mergerDigest, nil, ""
		return u, nil
	case u.mergerCredential != nil:
		u.command, u.commandDigest, u.credential, u.token = "", "", u.mergerCredential, ""
		return u, nil
	default:
		return upstream{}, diagnosticError("automatic merge requires a dedicated GitHub merger credential")
	}
}

func repositoryScope(repo, method, path string) (string, error) {
	if path == "/graphql" && method == http.MethodPost && ValidRepository(repo) {
		return repo, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/"), "/", 4)
	if len(parts) >= 3 && parts[0] == "repos" && ValidRepository(parts[1]+"/"+parts[2]) {
		return parts[1] + "/" + parts[2], nil
	}
	return "", diagnosticError("GitHub request requires a repository scope")
}

func configuredUpstream(c Config) (upstream, error) {
	if err := c.ValidateGitHubCommands(); err != nil {
		return upstream{}, err
	}
	up := newUpstream(c.GitHubToken)
	up.credential, up.mergerCredential = cloneCredential(c.GitHubReader), cloneCredential(c.GitHubMerger)
	up.command, up.commandDigest = c.GitHubCommand, c.GitHubCommandSHA256
	up.readArgs, up.mergeArgs = slices.Clone(c.GitHubReadArgs), slices.Clone(c.GitHubMergeArgs)
	up.mergerCommand, up.mergerDigest = c.GitHubMergerCommand, c.GitHubMergerSHA256
	if up.credential == nil && up.command == "" && up.token == "" && os.Getenv("INK_GITHUB_TOKEN") != "" {
		up.credential = &GitHubCredential{TokenEnv: "INK_GITHUB_TOKEN"}
	}
	for _, credential := range []*GitHubCredential{up.credential, up.mergerCredential} {
		if credential != nil && credential.TokenEnv != "" {
			up.secretEnv = append(up.secretEnv, credential.TokenEnv)
		}
	}
	return up, nil
}

func cloneCredential(c *GitHubCredential) *GitHubCredential {
	if c == nil {
		return nil
	}
	copy := *c
	if c.Helper != nil {
		helper := *c.Helper
		helper.Args = slices.Clone(c.Helper.Args)
		copy.Helper = &helper
	}
	if c.App != nil {
		app := *c.App
		copy.App = &app
	}
	return &copy
}

func (c GitHubCredential) sameIdentity(other GitHubCredential) bool {
	switch {
	case c.App != nil && other.App != nil:
		return c.App.AppID == other.App.AppID || c.App.InstallationID == other.App.InstallationID
	case c.Helper != nil && other.Helper != nil:
		return c.Helper.Identity == other.Helper.Identity ||
			(sameAppCommand(c.Helper.Command, other.Helper.Command) && slices.Equal(c.Helper.Args, other.Helper.Args))
	case c.TokenEnv != "" && other.TokenEnv != "":
		return c.TokenEnv == other.TokenEnv
	default:
		return false
	}
}

func (u upstream) resolveMerger(repo string, reuseReader bool, reviewerCommand string) (upstream, error) {
	merger, err := u.mergeRole(reuseReader)
	if err != nil {
		return upstream{}, err
	}
	command := merger.command
	if merger.credential != nil && merger.credential.Helper != nil {
		command = merger.credential.Helper.Command
	}
	if sameAppCommand(reviewerCommand, command) {
		return upstream{}, diagnosticError("reviewer and merger must use separate GitHub identities")
	}
	if !reuseReader && merger.credential != nil && merger.credential.TokenEnv != "" {
		reader := GitHubCredential{}
		if u.credential != nil {
			reader = *u.credential
		}
		if token := u.source(reader, u.token).pat(); token != "" && token == merger.source(*merger.credential, "").pat() {
			return upstream{}, diagnosticError("GitHub reader and merger PATs must be separate")
		}
	}
	merger.expectedIdentity, err = merger.mergeIdentity(repo)
	return merger, err
}

func (u upstream) compareMergeIdentity(repo, ref string) error {
	if u.expectedIdentity != "" && u.expectedIdentity != scopedMergeIdentity(u.apiHost(), repo, ref) {
		return diagnosticError("original GitHub merger identity is unavailable")
	}
	return nil
}

func (u upstream) checkMergeIdentity(repo string) error {
	ref, err := u.credentialReference(githubMerger)
	if err != nil {
		return err
	}
	return u.compareMergeIdentity(repo, ref)
}

func scopedMergeIdentity(host, repo, reference string) string {
	return fingerprint(host, strings.ToLower(repo), string(githubMerger), reference)
}
