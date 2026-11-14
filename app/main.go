// pg-backup: one service that backs up many Postgres databases to S3.
//
// One JSON state file, cron schedules in UTC, pg_dump clients 16/17/18
// picked per server, rclone for S3, server-rendered HTML.
//
// Access:
//   - default: joins the tailnet with tsnet as TS_HOSTNAME and serves only
//     there. The Tailscale login is the user; ALLOWED_USERS (required) limits
//     it. TS_AUTHKEY is needed on first start; node state then persists in
//     DATA_DIR/tailscale.
//   - DEV_NO_AUTH=1: plain HTTP on LISTEN_ADDR with no login (local dev only).
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

//go:embed templates/*.html static/*
var assets embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type App struct {
	store   *Store
	runner  *Runner
	sched   *Scheduler
	pages   map[string]*template.Template
	userOf  func(*http.Request) (string, error)
	allowed map[string]bool
	mode    string
}

func main() {
	dataDir := env("DATA_DIR", "/data")
	store, err := OpenStore(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	s3 := S3Config{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		Region:    env("S3_REGION", "us-east-1"),
		Provider:  env("S3_PROVIDER", "Other"),
		PathStyle: env("S3_FORCE_PATH_STYLE", "true"),
	}
	var missing []string
	for k, v := range map[string]string{"S3_ENDPOINT": s3.Endpoint, "S3_BUCKET": s3.Bucket, "S3_ACCESS_KEY_ID": s3.AccessKey, "S3_SECRET_ACCESS_KEY": s3.SecretKey} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("missing required env: %s", strings.Join(missing, " "))
	}
	parallel, _ := strconv.Atoi(env("MAX_PARALLEL", "2"))
	runner := NewRunner(store, s3, env("WORKDIR", "/tmp/pg-backup"), env("PG_LIBEXEC", "/usr/libexec"), max(parallel, 1))
	sched := NewScheduler(store, runner)
	sched.Reload()

	app := &App{store: store, runner: runner, sched: sched, pages: loadPages(), allowed: map[string]bool{}}
	for _, u := range strings.Split(os.Getenv("ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			app.allowed[u] = true
		}
	}

	if url := os.Getenv("DISCORD_WEBHOOK_URL"); url != "" {
		runner.OnFailure = discordNotifier(url)
	}

	var ln net.Listener
	if os.Getenv("DEV_NO_AUTH") != "1" {
		if len(app.allowed) == 0 {
			log.Fatal("ALLOWED_USERS is required (comma list of Tailscale logins); set DEV_NO_AUTH=1 only for local dev")
		}
		ts := &tsnet.Server{
			Hostname: env("TS_HOSTNAME", "pg-backup"),
			Dir:      filepath.Join(dataDir, "tailscale"),
			AuthKey:  os.Getenv("TS_AUTHKEY"),
		}
		if err := ts.Start(); err != nil {
			log.Fatal(err)
		}
		lc, err := ts.LocalClient()
		if err != nil {
			log.Fatal(err)
		}
		app.userOf = func(r *http.Request) (string, error) {
			who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				return "", err
			}
			return who.UserProfile.LoginName, nil
		}
		if env("TS_HTTPS", "true") == "true" {
			ln, err = ts.ListenTLS("tcp", ":443")
			app.mode = "tailscale https"
			// Plain http://pg-backup/ redirects to the HTTPS name.
			if plain, perr := ts.Listen("tcp", ":80"); perr == nil {
				go func() {
					_ = http.Serve(plain, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						host := ts.Hostname
						if d := ts.CertDomains(); len(d) > 0 {
							host = d[0]
						}
						http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusFound)
					}))
				}()
			}
		} else {
			ln, err = ts.Listen("tcp", ":80")
			app.mode = "tailscale http"
		}
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("serving on tailnet as %s (%s)", ts.Hostname, app.mode)
	} else {
		addr := env("LISTEN_ADDR", "127.0.0.1:8080")
		if !loopback(addr) && os.Getenv("DEV_NO_AUTH_ANY_ADDR") != "1" {
			log.Fatalf("DEV_NO_AUTH serves without login, so LISTEN_ADDR must be loopback (got %q). In a container, publish the port on 127.0.0.1 and set DEV_NO_AUTH_ANY_ADDR=1.", addr)
		}
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		app.userOf = func(*http.Request) (string, error) { return "local", nil }
		app.mode = "local, no login"
		log.Printf("serving on http://%s (no login, local dev only)", addr)
	}
	log.Printf("clients: Postgres %s", strings.Join(runner.Clients(), ", "))
	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

func loadPages() map[string]*template.Template {
	funcs := template.FuncMap{
		"bytes": humanBytes,
		"ago":   ago,
		"until": until,
		"ts":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"deref": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		"dur": func(a time.Time, b *time.Time) string {
			if b == nil {
				return ""
			}
			d := b.Sub(a).Round(100 * time.Millisecond)
			return d.String()
		},
	}
	pages := map[string]*template.Template{}
	names, _ := fs.Glob(assets, "templates/*.html")
	for _, n := range names {
		base := filepath.Base(n)
		if base == "layout.html" {
			continue
		}
		pages[base] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", n))
	}
	return pages
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /targets/new", a.newTarget)
	mux.HandleFunc("POST /targets", a.saveTarget)
	mux.HandleFunc("GET /targets/{id}", a.showTarget)
	mux.HandleFunc("GET /targets/{id}/edit", a.editTarget)
	mux.HandleFunc("POST /targets/{id}", a.saveTarget)
	mux.HandleFunc("POST /targets/{id}/delete", a.deleteTarget)
	mux.HandleFunc("POST /targets/{id}/backup", a.startJob(KindBackup))
	mux.HandleFunc("POST /targets/{id}/verify", a.startJob(KindVerify))
	mux.HandleFunc("GET /targets/{id}/restore", a.restoreForm)
	mux.HandleFunc("POST /targets/{id}/restore", a.startRestore)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /targets/{id}/test", a.testTarget)
	mux.HandleFunc("GET /runs/{id}", a.showRun)
	return a.auth(mux)
}

type ctxKey struct{}

func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := a.userOf(r)
		if err != nil {
			http.Error(w, "cannot identify you on the tailnet", http.StatusForbidden)
			return
		}
		if len(a.allowed) > 0 && !a.allowed[user] {
			http.Error(w, user+" is not allowed here", http.StatusForbidden)
			return
		}
		// Browsers send Sec-Fetch-Site; refuse cross-site form posts.
		if r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

func userOf(r *http.Request) string { s, _ := r.Context().Value(ctxKey{}).(string); return s }

type page struct {
	Title   string
	User    string
	Mode    string
	Bucket  string
	Flash   string
	Error   string
	Version string
	Refresh bool
	Data    any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	p := page{Version: version, Title: title, User: userOf(r), Mode: a.mode, Bucket: a.runner.s3.Bucket,
		Flash: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"), Data: data}
	if m, ok := data.(map[string]any); ok {
		p.Refresh, _ = m["Running"].(bool)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func back(w http.ResponseWriter, r *http.Request, to, key, msg string) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	http.Redirect(w, r, to+sep+key+"="+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

type row struct {
	T          Target
	Health     string // ok | failed | stale | new | off | running
	LastBackup *Run
	LastVerify *Run
	Next       time.Time
	HasNext    bool
	Running    bool
}

func (a *App) rowFor(t Target) row {
	rw := row{T: t, Running: a.runner.Busy(t.ID)}
	if b, ok := a.store.LastFinished(t.ID, "backup"); ok {
		rw.LastBackup = &b
	}
	if v, ok := a.store.LastFinished(t.ID, "verify"); ok {
		rw.LastVerify = &v
	}
	rw.Next, rw.HasNext = a.sched.Next(t.ID)
	switch {
	case rw.Running:
		rw.Health = "running"
	case !t.Enabled:
		rw.Health = "off"
	case rw.LastBackup == nil:
		rw.Health = "new"
	case rw.LastBackup.Status == "failed":
		rw.Health = "failed"
	case time.Since(rw.LastBackup.Started) > 26*time.Hour:
		rw.Health = "stale"
	default:
		rw.Health = "ok"
	}
	return rw
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	var rows []row
	counts := map[string]int{}
	for _, t := range a.store.Targets() {
		rw := a.rowFor(t)
		counts[rw.Health]++
		rows = append(rows, rw)
	}
	a.render(w, r, "dashboard.html", "Backups", map[string]any{
		"Rows": rows, "Counts": counts, "Clients": a.runner.Clients(), "Running": counts["running"] > 0,
	})
}

func (a *App) newTarget(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "form.html", "New database", Target{Port: 5432, Schedule: "0 3 * * *", RetentionDays: 14, Enabled: true})
}

func (a *App) editTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "form.html", "Edit "+t.Name, t)
}

func (a *App) saveTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var t Target
	if id != "" {
		old, ok := a.store.Target(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		t = old
	}
	f := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	t.Name = f("name")
	t.Host = f("host")
	t.Port, _ = strconv.Atoi(f("port"))
	t.Database = f("database")
	t.User = f("user")
	if p := r.FormValue("password"); p != "" {
		t.Password = p // blank keeps the stored password
	}
	t.Schedule = f("schedule")
	t.RetentionDays, _ = strconv.Atoi(f("retention"))
	t.Prefix = strings.Trim(f("prefix"), "/")
	if t.Prefix == "" {
		t.Prefix = t.Name
	}
	t.Enabled = r.FormValue("enabled") == "on"

	formURL := "/targets/new"
	if id != "" {
		formURL = "/targets/" + id + "/edit"
	}
	if err := validate(t); err != nil {
		back(w, r, formURL, "err", err.Error())
		return
	}
	if err := uniqueFiles(t, a.store.Targets()); err != nil {
		back(w, r, formURL, "err", err.Error())
		return
	}
	saved, err := a.store.PutTarget(t)
	if err != nil {
		back(w, r, formURL, "err", err.Error())
		return
	}
	a.sched.Reload()

	// Check the connection straight away so the operator sees the version.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, ver, err := a.runner.ServerVersion(ctx, saved)
	if err != nil {
		back(w, r, "/targets/"+saved.ID, "err", "Saved, but the connection failed: "+err.Error())
		return
	}
	a.store.SetDetected(saved.ID, ver)
	back(w, r, "/targets/"+saved.ID, "msg", "Saved. Connected to Postgres "+ver+".")
}

// uniqueFiles refuses two targets that would write the same
// <folder>/<database>-*.dump files, because each prunes that pattern.
func uniqueFiles(t Target, all []Target) error {
	for _, o := range all {
		if o.ID != t.ID && o.Prefix == t.Prefix && o.Database == t.Database {
			return fmt.Errorf("%s already writes %s/%s-*.dump; pick another S3 folder", o.Name, o.Prefix, o.Database)
		}
	}
	return nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validate(t Target) error {
	switch {
	case t.Name == "" || strings.ContainsAny(t.Name, " /"):
		return errors.New("name is required, without spaces or slashes")
	case t.Host == "":
		return errors.New("host is required")
	case t.Port <= 0:
		return errors.New("port must be a number")
	case t.Database == "", t.User == "":
		return errors.New("database and user are required")
	case t.RetentionDays <= 0:
		return errors.New("retention must be at least 1 day")
	}
	if _, err := cronParser.Parse(t.Schedule); err != nil {
		return fmt.Errorf("schedule: %v", err)
	}
	return nil
}

func (a *App) showTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	files, lerr := a.runner.List(ctx, t)
	listErr := ""
	if lerr != nil {
		listErr = lerr.Error()
	}
	rw := a.rowFor(t)
	a.render(w, r, "target.html", t.Name, map[string]any{
		"Row": rw, "T": t, "Files": files, "ListErr": listErr, "Runs": a.store.Runs(t.ID),
		"Remote": "s3://" + a.runner.s3.Bucket + "/" + t.Prefix, "Running": rw.Running,
	})
}

func (a *App) deleteTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != t.Name {
		back(w, r, "/targets/"+t.ID+"/edit", "err", "Type the name to confirm. Stored backups in S3 are kept.")
		return
	}
	_ = a.store.DeleteTarget(t.ID)
	a.sched.Reload()
	back(w, r, "/", "msg", "Removed "+t.Name+". Its backups stay in S3.")
}

func (a *App) startJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := a.store.Target(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		runID, err := a.runner.Start(t, kind, userOf(r), "")
		if err != nil {
			back(w, r, "/targets/"+t.ID, "err", err.Error())
			return
		}
		http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
	}
}

func (a *App) restoreForm(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	file := r.URL.Query().Get("file")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	files, err := a.runner.List(ctx, t)
	if err != nil {
		back(w, r, "/targets/"+t.ID, "err", "Could not list S3: "+err.Error())
		return
	}
	var found *RemoteFile
	for i := range files {
		if files[i].Name == file {
			found = &files[i]
		}
	}
	if found == nil {
		back(w, r, "/targets/"+t.ID, "err", "No such backup: "+file)
		return
	}
	a.render(w, r, "restore.html", "Restore "+t.Name, map[string]any{"T": t, "F": *found, "Running": false})
}

func (a *App) startRestore(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	file := r.FormValue("file")
	kind := KindRestoreCopy
	if r.FormValue("mode") == "replace" {
		kind = KindRestoreReplace
		if r.FormValue("confirm") != t.Database {
			back(w, r, "/targets/"+t.ID+"/restore?file="+template.URLQueryEscaper(file), "err",
				"Type the database name "+t.Database+" to replace the live database.")
			return
		}
	}
	runID, err := a.runner.Start(t, kind, userOf(r), file)
	if err != nil {
		back(w, r, "/targets/"+t.ID, "err", err.Error())
		return
	}
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

func (a *App) testTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := a.store.Target(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	major, ver, err := a.runner.ServerVersion(ctx, t)
	if err != nil {
		back(w, r, "/targets/"+t.ID, "err", "Connection failed: "+err.Error())
		return
	}
	a.store.SetDetected(t.ID, ver)
	client, cerr := a.runner.client(major, "pg_dump")
	if cerr != nil {
		back(w, r, "/targets/"+t.ID, "err", "Connected to Postgres "+ver+", but "+cerr.Error())
		return
	}
	back(w, r, "/targets/"+t.ID, "msg", fmt.Sprintf("Connected. Postgres %s, dumps with %s.", ver, filepath.Base(filepath.Dir(client))))
}

func (a *App) showRun(w http.ResponseWriter, r *http.Request) {
	run, ok := a.store.Run(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _ := a.store.Target(run.TargetID)
	a.render(w, r, "run.html", run.Kind+" "+t.Name, map[string]any{"Run": run, "T": t, "Running": run.Status == "running"})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func ago(t time.Time) string   { return short(time.Since(t)) + " ago" }
func until(t time.Time) string { return "in " + short(time.Until(t)) }
