package ink

import (
	"bufio"
	"io"
	"net/url"
	"strings"
)

func githubRepo(remote string) string {
	remote = strings.TrimSpace(remote)
	var path string
	if strings.HasPrefix(remote, "git@github.com:") {
		path = strings.TrimPrefix(remote, "git@github.com:")
	} else if u, err := url.Parse(remote); err == nil && (u.Scheme == "https" || u.Scheme == "ssh") && strings.EqualFold(u.Hostname(), "github.com") {
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git"))
	if !repoName.MatchString(path) {
		return ""
	}
	return path
}

func ReadPush(remote string, route Route, input io.Reader) ([]Push, error) {
	var pushes []Push
	repo := githubRepo(remote)
	if repo == "" || !route.valid() {
		return nil, nil
	}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 || !validSHA(fields[1]) || !strings.HasPrefix(fields[2], "refs/heads/") {
			continue
		}
		pushes = append(pushes, Push{Repo: repo, SHA: fields[1], Ref: strings.TrimPrefix(fields[2], "refs/heads/"), Route: route})
	}
	return pushes, scanner.Err()
}

func validSHA(sha string) bool {
	return shaPattern.MatchString(sha) && strings.Trim(sha, "0") != ""
}
