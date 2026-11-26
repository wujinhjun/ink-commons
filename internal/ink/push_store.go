//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

func pushCutoff(now time.Time) int64 {
	return now.Add(-7 * 24 * time.Hour).UnixNano()
}

func (s *stateStore) savePushes(pushes []Push, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ctx := context.Background()
	q := statedb.New(tx)
	if err := q.PrunePushes(ctx, pushCutoff(now)); err != nil {
		return err
	}
	for _, p := range pushes {
		err := q.PutPush(ctx, statedb.PutPushParams{
			Repo: p.Repo, Ref: p.Ref, Sha: p.SHA, Route: string(jsonValue(p.Route)), ObservedAt: p.At.UnixNano(),
		})
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *stateStore) matchOrigin(pull Pull, now time.Time) (*execution.Route, error) {
	if pull.Head.Repo == nil || !validSHA(pull.Head.SHA) {
		return nil, nil
	}
	routes, err := statedb.New(s.db).MatchPushRoutes(context.Background(), statedb.MatchPushRoutesParams{
		Repo: strings.ToLower(pull.Head.Repo.FullName), Ref: pull.Head.Ref, Sha: pull.Head.SHA, ObservedAt: pushCutoff(now),
	})
	if err != nil || len(routes) != 1 {
		return nil, err
	}
	var route execution.Route
	if err := json.Unmarshal([]byte(routes[0]), &route); err != nil {
		return nil, err
	}
	return &route, nil
}

func decodePush(row statedb.Push) (Push, error) {
	push := Push{Repo: row.Repo, Ref: row.Ref, SHA: row.Sha, At: time.Unix(0, row.ObservedAt).UTC()}
	err := json.Unmarshal([]byte(row.Route), &push.Route)
	return push, err
}
