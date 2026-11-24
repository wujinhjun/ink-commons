package ccconnect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
)

// Handler exposes only the requested provider namespace. It has no Ink socket
// client and deliberately implements no managed execution operations.
func (e *Executor) Handler(provider, token string) http.Handler {
	return httpagent.Handler(token, func(ctx context.Context, req httpagent.Request) (httpagent.Response, error) {
		if req.Route.Provider != provider {
			return httpagent.Response{}, httpagent.Problem(http.StatusBadRequest)
		}
		res, err := e.handle(ctx, req)
		if err != nil {
			var network net.Error
			switch {
			case errors.Is(err, errAllocationMissing):
				return res, httpagent.Problem(http.StatusNotFound)
			case errors.Is(err, errAllocationAmbiguous):
				return res, httpagent.Problem(http.StatusConflict)
			case errors.As(err, &network) && network.Timeout():
				return res, httpagent.Problem(http.StatusGatewayTimeout)
			}
			var status execution.HTTPStatusError
			if errors.As(err, &status) {
				switch int(status) {
				case http.StatusForbidden, http.StatusTooManyRequests, http.StatusNotFound:
					return res, httpagent.Problem(status)
				}
			}
		}
		return res, err
	})
}

func (e *Executor) handle(ctx context.Context, r httpagent.Request) (res httpagent.Response, err error) {
	switch r.Operation {
	case "placement":
		res.Placement, err = e.ResolveReviewPlacement(ctx, r.Route, *r.Task)
	case "prepare":
		if r.Allocation == nil || r.Route.Session != "" || e.discordToken == "" {
			return res, httpagent.Problem(http.StatusBadRequest)
		}
		a := *r.Allocation
		if a.Name == "" {
			a.Name = e.PlanConversation(execution.ReviewPlacement{Parent: a.Parent}, *r.Task, a.ID).Name
		}
		if r.Mode == "find" {
			res.Session, err = e.FindConversation(ctx, a)
		} else {
			res.Session, err = e.CreateConversation(ctx, a)
		}
	case "conversation_context":
		var page execution.ConversationPage
		page, err = e.readContext(ctx, r.Route, *r.Task, r.Before)
		res.Conversation = &page
	case "deliver", "wake", "notify", "blocked_notice":
		if err := e.Validate(r.Route, false); err != nil {
			return res, httpagent.Problem(http.StatusBadRequest)
		}
		switch r.Operation {
		case "deliver":
			err = e.Deliver(ctx, r.Route, *r.Task)
		case "wake":
			err = e.WakeTask(ctx, r.Route, *r.Task)
		case "notify":
			err = e.Notify(ctx, r.Route, *r.Notification, *r.RequireACK)
		case "blocked_notice":
			err = e.NotifyTaskBlocked(ctx, r.Route.Session, *r.Task, r.Reason)
		}
		res.Status = "accepted"
	default:
		err = httpagent.Problem(http.StatusNotImplemented)
	}
	return res, err
}

func (e *Executor) readContext(ctx context.Context, route execution.Route, task execution.Delivery, before string) (execution.ConversationPage, error) {
	cutoff, err := time.Parse(time.RFC3339Nano, task.ContextBefore)
	if err != nil || cutoff.UnixMilli() <= 1420070400000 {
		return execution.ConversationPage{}, fmt.Errorf("invalid conversation cutoff")
	}
	boundary := uint64(cutoff.UnixMilli()-1420070400000) << 22
	cursor := boundary
	if before != "" {
		cursor, err = strconv.ParseUint(before, 10, 64)
		if err != nil || cursor == 0 || cursor > boundary {
			return execution.ConversationPage{}, httpagent.Problem(http.StatusBadRequest)
		}
	}
	page, err := e.ReadConversation(ctx, route, strconv.FormatUint(cursor, 10))
	page.TaskID, page.SHA, page.ReviewBefore = task.TaskID, task.SHA, task.ContextBefore
	page.Before = before
	return page, err
}
