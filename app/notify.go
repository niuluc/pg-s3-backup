package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// discordNotifier posts one short message per failed job to a Discord webhook.
func discordNotifier(url string) func(t Target, run Run) {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(t Target, run Run) {
		msg := fmt.Sprintf("pg-backup: %s of **%s** failed (trigger: %s)\n> %s", run.Kind, t.Name, run.Trigger, run.Message)
		body, _ := json.Marshal(map[string]string{"content": msg})
		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("discord notify failed: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("discord notify failed: HTTP %d", resp.StatusCode)
		}
	}
}
