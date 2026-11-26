package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackNotifierConfiguration(t *testing.T) {
	for _, c := range []struct {
		name, token, channel string
		enabled              bool
	}{
		{"disabled", "", "", false},
		{"missing token", "", "C0123456789", false},
		{"missing channel", "test-bot-token", "", false},
		{"enabled", "test-bot-token", "C0123456789", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := slackNotifier(c.token, c.channel) != nil; got != c.enabled {
				t.Errorf("notifications enabled = %v, want %v", got, c.enabled)
			}
		})
	}
}

func TestSlackNotifier(t *testing.T) {
	const token = "test-bot-token"
	const channel = "C0123456789"
	for _, c := range []struct {
		name          string
		status        int
		response      string
		transportFail bool
		wantLog       string
	}{
		{name: "success", status: 200, response: `{"ok":true}`},
		{name: "API rejection", status: 200, response: `{"ok":false,"error":"test-bot-token"}`, wantLog: "API rejected message"},
		{name: "missing acknowledgement", status: 200, response: `{}`, wantLog: "API rejected message"},
		{name: "malformed JSON", status: 200, response: `test-bot-token`, wantLog: "invalid response"},
		{name: "wrong acknowledgement type", status: 200, response: `{"ok":"test-bot-token"}`, wantLog: "invalid response"},
		{name: "empty response", status: 204, wantLog: "invalid response"},
		{name: "HTTP failure", status: 500, response: `{"ok":true,"error":"test-bot-token"}`, wantLog: "HTTP 500"},
		{name: "rate limited", status: 429, response: `test-bot-token`, wantLog: "HTTP 429"},
		{name: "redirect refused", status: 302, response: `test-bot-token`, wantLog: "HTTP 302"},
		{name: "transport failure", transportFail: true, wantLog: "request failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			oldOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(oldOutput) })

			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/api/chat.postMessage" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("missing bot authorization")
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
				}
				want := map[string]string{
					"channel": channel,
					"text":    "pg-backup: verify of **shop** failed (trigger: schedule)\n> connection refused\ntry again",
				}
				if !reflect.DeepEqual(payload, want) {
					t.Errorf("payload = %v, want %v", payload, want)
				}
				w.Header().Set("Location", "/unexpected")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.response))
			}))
			t.Cleanup(server.Close)
			localURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			attempts := 0
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				if r.URL.String() != "https://slack.com/api/chat.postMessage" {
					t.Errorf("unexpected Slack endpoint: %s", r.URL)
				}
				deadline, ok := r.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining <= 0 || remaining > 10*time.Second {
					t.Error("request must have a deadline within ten seconds")
				}
				if c.transportFail {
					return nil, errors.New("transport error containing " + token)
				}
				local := r.Clone(r.Context())
				local.URL.Scheme, local.URL.Host = localURL.Scheme, localURL.Host
				return original.RoundTrip(local)
			})
			t.Cleanup(func() { http.DefaultTransport = original })

			slackNotifier(token, channel)(Target{Name: "shop", Password: "test-db-password"}, Run{
				Kind: KindVerify, Trigger: "schedule", Message: "connection refused\ntry again",
			})
			if attempts != 1 {
				t.Errorf("request attempts = %d, want 1", attempts)
			}
			wantRequests := 1
			if c.transportFail {
				wantRequests = 0
			}
			if requests != wantRequests {
				t.Errorf("HTTP requests = %d, want %d", requests, wantRequests)
			}
			if c.wantLog == "" {
				if logs.Len() != 0 {
					t.Errorf("successful notification logged an error: %s", logs.String())
				}
			} else if !strings.Contains(logs.String(), "slack notify failed: "+c.wantLog) {
				t.Errorf("log = %q, want %q", logs.String(), c.wantLog)
			}
			for _, secret := range []string{token, "test-db-password"} {
				if strings.Contains(logs.String(), secret) {
					t.Error("notification log contains a credential")
				}
			}
		})
	}
}
