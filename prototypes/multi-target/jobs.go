package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// S3Config is shared by every target. Each target writes under its own prefix.
type S3Config struct {
	Endpoint, Bucket, AccessKey, SecretKey, Region, Provider, PathStyle string
}

func (c S3Config) env() []string {
	return []string{
		"RCLONE_CONFIG=/dev/null",
		"RCLONE_S3_PROVIDER=" + c.Provider,
		"RCLONE_S3_ACCESS_KEY_ID=" + c.AccessKey,
		"RCLONE_S3_SECRET_ACCESS_KEY=" + c.SecretKey,
		"RCLONE_S3_ENDPOINT=" + c.Endpoint,
		"RCLONE_S3_REGION=" + c.Region,
		"RCLONE_S3_FORCE_PATH_STYLE=" + c.PathStyle,
	}
}

func (c S3Config) remote(prefix string) string {
	return fmt.Sprintf(":s3:%s/%s", c.Bucket, strings.Trim(prefix, "/"))
}

// Runner executes jobs. One job per target at a time, MaxParallel overall.
type Runner struct {
	store   *Store
	s3      S3Config
	workdir string
	libexec string
	busy    sync.Map
	slots   chan struct{}
}

func NewRunner(store *Store, s3 S3Config, workdir, libexec string, maxParallel int) *Runner {
	_ = os.MkdirAll(workdir, 0o700)
	return &Runner{store: store, s3: s3, workdir: workdir, libexec: libexec, slots: make(chan struct{}, maxParallel)}
}

func (r *Runner) Busy(targetID string) bool {
	_, ok := r.busy.Load(targetID)
	return ok
}

var ErrBusy = errors.New("a job is already running for this target")

// Start launches a job in the background and returns the run id.
func (r *Runner) Start(t Target, kind, trigger string) (string, error) {
	if _, loaded := r.busy.LoadOrStore(t.ID, true); loaded {
		return "", ErrBusy
	}
	run := &Run{ID: newID(), TargetID: t.ID, Kind: kind, Trigger: trigger, Status: "running", Started: time.Now().UTC()}
	r.store.AddRun(run)
	go func() {
		defer r.busy.Delete(t.ID)
		r.slots <- struct{}{}
		defer func() { <-r.slots }()

		j := &job{r: r, t: t, runID: run.ID}
		var err error
		switch kind {
		case "backup":
			err = j.backup()
		case "verify":
			err = j.verify()
		default:
			err = fmt.Errorf("unknown job kind %q", kind)
		}
		now := time.Now().UTC()
		r.store.UpdateRun(run.ID, func(rr *Run) {
			rr.Finished = &now
			if err != nil {
				rr.Status = "failed"
				rr.Message = err.Error()
			} else {
				rr.Status = "ok"
			}
		})
		if err != nil {
			j.logf("ERROR: %v", err)
		}
	}()
	return run.ID, nil
}

type job struct {
	r     *Runner
	t     Target
	runID string
}

func (j *job) logf(format string, a ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
	j.r.store.UpdateRun(j.runID, func(rr *Run) { rr.Log += line })
}

func (j *job) set(fn func(rr *Run)) { j.r.store.UpdateRun(j.runID, fn) }

// exec runs a command with Postgres and rclone env. Secrets go through env,
// never argv. Output is appended to the run log.
func (j *job) exec(ctx context.Context, name string, args ...string) (string, error) {
	return runCmd(ctx, j.t, j.r.s3, func(s string) {
		j.r.store.UpdateRun(j.runID, func(rr *Run) { rr.Log += s })
	}, name, args...)
}

func runCmd(ctx context.Context, t Target, s3 S3Config, logOut func(string), name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), s3.env()...)
	cmd.Env = append(cmd.Env, "PGPASSWORD="+t.Password, "PGCONNECT_TIMEOUT=10", "PGAPPNAME=pg-backup")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if logOut != nil && stderr.Len() > 0 {
		logOut(stderr.String())
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return stdout.String(), fmt.Errorf("%s: %v %s", filepath.Base(name), err, msg)
	}
	return stdout.String(), nil
}

// ServerVersion asks the server for server_version_num, e.g. 160004 -> 16, "16.4".
func (r *Runner) ServerVersion(ctx context.Context, t Target) (int, string, error) {
	psql, err := r.client(18, "psql") // any recent psql can ask
	if err != nil {
		psql = "psql"
	}
	out, err := runCmd(ctx, t, r.s3, nil, psql, "-X", "-A", "-t",
		"-h", t.Host, "-p", strconv.Itoa(t.Port), "-U", t.User, "-d", t.Database,
		"-c", "show server_version_num")
	if err != nil {
		return 0, "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, "", fmt.Errorf("unexpected server_version_num %q", strings.TrimSpace(out))
	}
	major := n / 10000
	return major, fmt.Sprintf("%d.%d", major, n%100), nil
}

// client picks the client binary for a server major. Same major first, then
// the nearest newer one. Matching the major keeps dumps restorable on the
// same server (a PG18 pg_dump writes SET transaction_timeout, which PG16 rejects).
func (r *Runner) client(major int, bin string) (string, error) {
	for m := major; m <= major+5; m++ {
		p := filepath.Join(r.libexec, fmt.Sprintf("postgresql%d", m), bin)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no %s client for Postgres %d (have: %s)", bin, major, strings.Join(r.Clients(), ", "))
}

// Clients lists installed client majors.
func (r *Runner) Clients() []string {
	matches, _ := filepath.Glob(filepath.Join(r.libexec, "postgresql*", "pg_dump"))
	var out []string
	for _, m := range matches {
		v := strings.TrimPrefix(filepath.Base(filepath.Dir(m)), "postgresql")
		if _, err := strconv.Atoi(v); err == nil {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func (j *job) connArgs(db string) []string {
	return []string{"-h", j.t.Host, "-p", strconv.Itoa(j.t.Port), "-U", j.t.User, "-d", db}
}

func (j *job) detect(ctx context.Context) (int, error) {
	major, ver, err := j.r.ServerVersion(ctx, j.t)
	if err != nil {
		return 0, fmt.Errorf("connect %s:%d: %w", j.t.Host, j.t.Port, err)
	}
	j.r.store.SetDetected(j.t.ID, ver)
	j.set(func(rr *Run) { rr.ServerVersion = ver })
	return major, nil
}

func (j *job) backup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	major, err := j.detect(ctx)
	if err != nil {
		return err
	}
	pgDump, err := j.r.client(major, "pg_dump")
	if err != nil {
		return err
	}
	pgRestore, _ := j.r.client(major, "pg_restore")
	j.set(func(rr *Run) { rr.Client = filepath.Base(filepath.Dir(pgDump)) })
	j.logf("server Postgres %d, using %s", major, pgDump)

	file := fmt.Sprintf("%s-%s.dump", j.t.Database, time.Now().UTC().Format("20060102T150405Z"))
	tmp := filepath.Join(j.r.workdir, j.t.ID+"-"+file)
	defer os.Remove(tmp)

	j.logf("dumping %s", j.t.Database)
	args := append([]string{"-Fc", "--no-acl", "--no-owner", "-f", tmp}, j.connArgs(j.t.Database)...)
	if _, err := j.exec(ctx, pgDump, args...); err != nil {
		return err
	}
	// Integrity gate: a dump pg_restore cannot read is not a backup.
	if _, err := j.exec(ctx, pgRestore, "--list", tmp); err != nil {
		return fmt.Errorf("dump failed integrity check: %w", err)
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	j.set(func(rr *Run) { rr.File = file; rr.Size = st.Size() })
	j.logf("dump ok, %s; uploading to %s/", humanBytes(st.Size()), j.r.s3.remote(j.t.Prefix))

	remote := j.r.s3.remote(j.t.Prefix)
	if _, err := j.exec(ctx, "rclone", "copyto", tmp, remote+"/"+file, "--s3-no-check-bucket"); err != nil {
		return err
	}
	// Prune only files this target writes: <database>-*.dump.
	j.logf("pruning %s-*.dump older than %dd", j.t.Database, j.t.RetentionDays)
	if _, err := j.exec(ctx, "rclone", "delete", remote+"/", "--min-age", fmt.Sprintf("%dd", j.t.RetentionDays),
		"--include", j.t.Database+"-*.dump", "--s3-no-check-bucket"); err != nil {
		j.logf("prune failed (backup is still stored): %v", err)
	}
	j.logf("backup complete: %s", file)
	return nil
}

func (j *job) verify() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	files, err := j.r.List(ctx, j.t)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no backups to verify")
	}
	name := files[0].Name
	j.set(func(rr *Run) { rr.File = name; rr.Size = files[0].Size })

	major, err := j.detect(ctx)
	if err != nil {
		return err
	}
	pgRestore, err := j.r.client(major, "pg_restore")
	if err != nil {
		return err
	}
	psql, _ := j.r.client(major, "psql")
	j.set(func(rr *Run) { rr.Client = filepath.Base(filepath.Dir(pgRestore)) })

	tmp := filepath.Join(j.r.workdir, j.t.ID+"-verify-"+name)
	defer os.Remove(tmp)
	j.logf("downloading %s", name)
	if _, err := j.exec(ctx, "rclone", "copyto", j.r.s3.remote(j.t.Prefix)+"/"+name, tmp, "--s3-no-check-bucket"); err != nil {
		return err
	}

	scratch := fmt.Sprintf("%s_verify_%s", j.t.Database, time.Now().UTC().Format("20060102150405"))
	j.logf("creating throwaway database %s", scratch)
	if _, err := j.exec(ctx, psql, append([]string{"-X", "-c", fmt.Sprintf("CREATE DATABASE %q", scratch)}, j.connArgs(j.t.Database)...)...); err != nil {
		return err
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := j.exec(dctx, psql, append([]string{"-X", "-c", fmt.Sprintf("DROP DATABASE IF EXISTS %q", scratch)}, j.connArgs(j.t.Database)...)...); err != nil {
			j.logf("could not drop %s: %v", scratch, err)
		} else {
			j.logf("dropped %s", scratch)
		}
	}()

	j.logf("restoring into %s", scratch)
	args := append([]string{"--no-owner", "--no-acl", "--exit-on-error"}, j.connArgs(scratch)...)
	if _, err := j.exec(ctx, pgRestore, append(args, tmp)...); err != nil {
		return err
	}
	out, err := j.exec(ctx, psql, append([]string{"-X", "-A", "-t", "-c",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')"},
		j.connArgs(scratch)...)...)
	if err != nil {
		return err
	}
	tables, _ := strconv.Atoi(strings.TrimSpace(out))
	if tables == 0 {
		return errors.New("restored database has 0 tables")
	}
	j.logf("verify ok: %s restored with %d tables", name, tables)
	j.set(func(rr *Run) { rr.Message = fmt.Sprintf("%d tables restored", tables) })
	return nil
}

// RemoteFile is one object in a target's prefix.
type RemoteFile struct {
	Name    string    `json:"Name"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
}

// List returns the target's dumps, newest first.
func (r *Runner) List(ctx context.Context, t Target) ([]RemoteFile, error) {
	out, err := runCmd(ctx, t, r.s3, nil, "rclone", "lsjson", r.s3.remote(t.Prefix)+"/", "--files-only",
		"--include", t.Database+"-*.dump", "--s3-no-check-bucket")
	if err != nil {
		return nil, err
	}
	var files []RemoteFile
	if err := json.Unmarshal([]byte(out), &files); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files, nil
}

// Scheduler maps each enabled target to a cron entry (UTC).
type Scheduler struct {
	mu      sync.Mutex
	c       *cron.Cron
	entries map[string]cron.EntryID
	store   *Store
	runner  *Runner
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func NewScheduler(store *Store, runner *Runner) *Scheduler {
	s := &Scheduler{
		c:       cron.New(cron.WithLocation(time.UTC), cron.WithParser(cronParser)),
		entries: map[string]cron.EntryID{},
		store:   store,
		runner:  runner,
	}
	s.c.Start()
	return s
}

func (s *Scheduler) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		s.c.Remove(e)
		delete(s.entries, id)
	}
	for _, t := range s.store.Targets() {
		if !t.Enabled {
			continue
		}
		id := t.ID
		e, err := s.c.AddFunc(t.Schedule, func() {
			cur, ok := s.store.Target(id)
			if !ok || !cur.Enabled {
				return
			}
			_, _ = s.runner.Start(cur, "backup", "schedule")
		})
		if err == nil {
			s.entries[id] = e
		}
	}
}

func (s *Scheduler) Next(targetID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[targetID]
	if !ok {
		return time.Time{}, false
	}
	return s.c.Entry(e).Next, true
}
