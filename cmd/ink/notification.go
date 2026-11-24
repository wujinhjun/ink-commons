//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/execution"
)

func notificationCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action}
	flags := command.Flags()
	event := flags.String("event", "", "event identifier from the notification")

	command.RunE = func(_ *cobra.Command, args []string) error {
		if *event == "" || len(args) != 0 {
			return errors.New("usage: ink notification ack|status --event EVENT_ID")
		}
		dir, err := options.directory()
		if err != nil {
			return err
		}
		if action == "status" {
			res, err := localClient(dir, 10*time.Second).Get("http://ink/v1/notifications/status?event_id=" + url.QueryEscape(*event))
			if err != nil {
				return errors.New("daemon unavailable")
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon HTTP %d", res.StatusCode)
			}
			var status struct {
				EventID           string                 `json:"event_id"`
				Event             string                 `json:"event"`
				SHA               string                 `json:"sha"`
				Head              string                 `json:"current_head_sha"`
				Phase             string                 `json:"phase"`
				NotificationState string                 `json:"notification_state"`
				Merge             *execution.MergeStatus `json:"merge"`
			}
			if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&status); err != nil {
				return fmt.Errorf("invalid notification status: %w", err)
			}
			if status.EventID != *event || status.Merge == nil || status.Merge.State == "" {
				return errors.New("invalid notification status")
			}
			return json.NewEncoder(os.Stdout).Encode(status)
		}
		payload, err := json.Marshal(map[string]string{"event_id": *event})
		if err != nil {
			return err
		}
		res, err := localClient(dir, 10*time.Second).Post("http://ink/v1/notifications/ack", "application/json", bytes.NewReader(payload))
		if err != nil {
			return errors.New("daemon unavailable")
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("daemon HTTP %d: %s", res.StatusCode, body)
		}
		var receipt struct {
			Status string `json:"status"`
			First  bool   `json:"first"`
		}
		if err := json.Unmarshal(body, &receipt); err != nil {
			return fmt.Errorf("invalid notification receipt: %w", err)
		}
		if receipt.Status != "acknowledged" && receipt.Status != "superseded" {
			return errors.New("unexpected notification receipt status")
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	}
	return command
}
