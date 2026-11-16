package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func notificationCommand(args []string) error {
	if len(args) == 0 || args[0] != "ack" {
		return errors.New("usage: ink notification ack --event EVENT_ID")
	}
	flags := flag.NewFlagSet("notification ack", flag.ContinueOnError)
	event := flags.String("event", "", "approval event identifier from the notification")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *event == "" || flags.NArg() != 0 {
		return errors.New("usage: ink notification ack --event EVENT_ID")
	}
	dir, err := directory()
	if err != nil {
		return err
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
