package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := Target{Name: "umami", Host: "db", Port: 5432, Database: "umami", User: "u", Schedule: "0 3 * * *", RetentionDays: 14}
	if err := validate(ok); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	cases := map[string]func(*Target){
		"space in name": func(t *Target) { t.Name = "a b" },
		"slash in name": func(t *Target) { t.Name = "a/b" },
		"no host":       func(t *Target) { t.Host = "" },
		"no port":       func(t *Target) { t.Port = 0 },
		"no database":   func(t *Target) { t.Database = "" },
		"no retention":  func(t *Target) { t.RetentionDays = 0 },
		"bad cron":      func(t *Target) { t.Schedule = "every night" },
		"six fields":    func(t *Target) { t.Schedule = "0 0 3 * * *" },
	}
	for name, mut := range cases {
		bad := ok
		mut(&bad)
		if validate(bad) == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	for _, s := range []string{"@every 6h", "@hourly", "*/15 * * * *"} {
		good := ok
		good.Schedule = s
		if err := validate(good); err != nil {
			t.Errorf("schedule %q rejected: %v", s, err)
		}
	}
}

func TestClientPicksSameMajorThenNewer(t *testing.T) {
	dir := t.TempDir()
	for _, m := range []string{"16", "18"} {
		p := filepath.Join(dir, "postgresql"+m)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "pg_dump"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A non-numeric dir (Alpine's default symlink dir) must not count.
	_ = os.MkdirAll(filepath.Join(dir, "postgresql"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "postgresql", "pg_dump"), nil, 0o755)

	r := &Runner{libexec: dir}
	want := map[int]string{16: "postgresql16", 17: "postgresql18", 18: "postgresql18"}
	for major, wantDir := range want {
		got, err := r.client(major, "pg_dump")
		if err != nil {
			t.Fatalf("major %d: %v", major, err)
		}
		if filepath.Base(filepath.Dir(got)) != wantDir {
			t.Errorf("major %d: got %s, want %s", major, got, wantDir)
		}
	}
	if _, err := r.client(19, "pg_dump"); err == nil {
		t.Error("major 19: expected an error, no client is new enough")
	}
	if got := strings.Join(r.Clients(), ","); got != "16,18" {
		t.Errorf("Clients() = %q, want 16,18", got)
	}
}

func TestStoreKeepsPasswordAndTrimsRuns(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tg, err := s.PutTarget(Target{Name: "a", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < runsPerTarget+5; i++ {
		s.AddRun(&Run{ID: newID(), TargetID: tg.ID, Kind: KindBackup, Status: "ok", Started: time.Now()})
	}
	s.AddRun(&Run{ID: "live", TargetID: tg.ID, Kind: KindBackup, Status: "running", Started: time.Now()})
	if n := len(s.Runs(tg.ID)); n != runsPerTarget {
		t.Errorf("runs kept = %d, want %d", n, runsPerTarget)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state.json mode = %v, want 0600", info.Mode().Perm())
	}

	// Reopen: the password survives and a running job is marked failed.
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s2.Target(tg.ID)
	if got.Password != "secret" {
		t.Error("password lost on reload")
	}
	r, _ := s2.Run("live")
	if r.Status != "failed" {
		t.Errorf("interrupted run status = %q, want failed", r.Status)
	}
}

func TestAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(userOf(r))) })
	cases := []struct {
		name    string
		user    string
		err     error
		allowed []string
		method  string
		site    string
		code    int
	}{
		{"allowed user", "kuba@github", nil, []string{"kuba@github"}, "GET", "", 200},
		{"other user", "eve@github", nil, []string{"kuba@github"}, "GET", "", 403},
		{"unknown peer", "", errors.New("no peer"), []string{"kuba@github"}, "GET", "", 403},
		{"cross-site post", "kuba@github", nil, []string{"kuba@github"}, "POST", "cross-site", 403},
		{"same-origin post", "kuba@github", nil, []string{"kuba@github"}, "POST", "same-origin", 200},
	}
	for _, c := range cases {
		a := &App{allowed: map[string]bool{}}
		for _, u := range c.allowed {
			a.allowed[u] = true
		}
		user, uerr := c.user, c.err
		a.userOf = func(*http.Request) (string, error) { return user, uerr }
		req := httptest.NewRequest(c.method, "/", nil)
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		a.auth(ok).ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s: code %d, want %d", c.name, rec.Code, c.code)
		}
	}
}

func TestPagesRender(t *testing.T) {
	pages := loadPages()
	for _, name := range []string{"dashboard.html", "form.html", "target.html", "run.html", "restore.html"} {
		if pages[name] == nil {
			t.Errorf("page %s not loaded", name)
		}
	}
}
