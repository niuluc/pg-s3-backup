package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func slackNotifier(token, channel string) func(t Target, run Run) {
	if token == "" || channel == "" {
		return nil
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return func(t Target, run Run) {
		msg := fmt.Sprintf("pg-backup: %s of **%s** failed (trigger: %s)\n> %s", run.Kind, t.Name, run.Trigger, run.Message)
		body, _ := json.Marshal(map[string]string{"channel": channel, "text": msg})
		req, err := http.NewRequest(http.MethodPost, "https://slack.com/api/chat.postMessage", bytes.NewReader(body))
		if err != nil {
			log.Print("slack notify failed: cannot create request")
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Print("slack notify failed: request failed")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Printf("slack notify failed: HTTP %d", resp.StatusCode)
			return
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			log.Print("slack notify failed: invalid response")
			return
		}
		if !result.OK {
			log.Print("slack notify failed: API rejected message")
		}
	}
}
