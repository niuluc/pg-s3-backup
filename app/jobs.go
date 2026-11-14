package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	// OnFailure is called after a job fails. Optional.
	OnFailure func(t Target, run Run)
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

// Job kinds.
const (
	KindBackup         = "backup"
	KindVerify         = "verify"
	KindRestoreCopy    = "restore-copy"
	KindRestoreReplace = "restore-replace"
)

// Start launches a job in the background and returns the run id. file names
// the dump for restore jobs; it is ignored by backup and verify.
func (r *Runner) Start(t Target, kind, trigger, file string) (string, error) {
	if _, loaded := r.busy.LoadOrStore(t.ID, true); loaded {
		return "", ErrBusy
	}
	run := &Run{ID: newID(), TargetID: t.ID, Kind: kind, Trigger: trigger, Status: "running", Started: time.Now().UTC(), File: file}
	r.store.AddRun(run)
	go func() {
		defer r.busy.Delete(t.ID)
		r.slots <- struct{}{}
		defer func() { <-r.slots }()

		j := &job{r: r, t: t, runID: run.ID}
		var err error
		switch kind {
		case KindBackup:
			err = j.backup()
		case KindVerify:
			err = j.verify()
		case KindRestoreCopy:
			err = j.restoreCopy(file)
		case KindRestoreReplace:
			err = j.restoreReplace(file)
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
			log.Printf("job failed: target=%s kind=%s run=%s: %v", t.Name, kind, run.ID, err)
			if r.OnFailure != nil {
				if done, ok := r.store.Run(run.ID); ok {
					r.OnFailure(t, done)
				}
			}
		} else {
			log.Printf("job ok: target=%s kind=%s run=%s", t.Name, kind, run.ID)
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

// replaceStream feeds psql: BEGIN, pre, then pg_restore's SQL, then COMMIT
// only if pg_restore exited 0. If pg_restore fails or psql stops on an error,
// psql never sees COMMIT and the server rolls the whole transaction back.
func (j *job) replaceStream(ctx context.Context, restore, psql []string, pre string) error {
	env := append(os.Environ(), j.r.s3.env()...)
	env = append(env, "PGPASSWORD="+j.t.Password, "PGCONNECT_TIMEOUT=10", "PGAPPNAME=pg-backup")
	ca := exec.CommandContext(ctx, restore[0], restore[1:]...)
	cb := exec.CommandContext(ctx, psql[0], psql[1:]...)
	ca.Env, cb.Env = env, env
	var ea, eb bytes.Buffer
	ca.Stderr, cb.Stderr = &ea, &eb
	aOut, err := ca.StdoutPipe()
	if err != nil {
		return err
	}
	bIn, err := cb.StdinPipe()
	if err != nil {
		return err
	}
	if err := cb.Start(); err != nil {
		return err
	}
	defer func() {
		for _, e := range []*bytes.Buffer{&ea, &eb} {
			if e.Len() > 0 {
				j.r.store.UpdateRun(j.runID, func(rr *Run) { rr.Log += e.String() })
			}
		}
	}()
	fail := func(err error) error {
		bIn.Close() // EOF without COMMIT: the server rolls back
		_ = cb.Wait()
		return err
	}
	if err := ca.Start(); err != nil {
		return fail(err)
	}
	_, werr := io.WriteString(bIn, "BEGIN;\n"+pre)
	if werr == nil {
		_, werr = io.Copy(bIn, aOut)
	}
	if werr != nil {
		// psql went away (it stops on the first error). Stop pg_restore too.
		_ = ca.Process.Kill()
		_ = ca.Wait()
		bIn.Close()
		errB := cb.Wait()
		return fmt.Errorf("psql: %v %s", errB, lastLine(eb.String()))
	}
	if errA := ca.Wait(); errA != nil {
		return fail(fmt.Errorf("pg_restore: %v %s", errA, lastLine(ea.String())))
	}
	if _, err := io.WriteString(bIn, "COMMIT;\n"); err != nil {
		return fail(fmt.Errorf("psql: %v %s", err, lastLine(eb.String())))
	}
	bIn.Close()
	if errB := cb.Wait(); errB != nil {
		return fmt.Errorf("psql: %v %s", errB, lastLine(eb.String()))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// safeName accepts only plain dump basenames, so an S3 object name can never
// steer a local path out of WORKDIR.
func safeName(name string) bool {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".dump") {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
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
	_, err := j.dumpAndUpload(ctx)
	return err
}

// dumpAndUpload takes a fresh dump, uploads it, prunes old ones and returns
// the file name.
func (j *job) dumpAndUpload(ctx context.Context) (string, error) {
	major, err := j.detect(ctx)
	if err != nil {
		return "", err
	}
	pgDump, err := j.r.client(major, "pg_dump")
	if err != nil {
		return "", err
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
		return "", err
	}
	// Integrity gate: a dump pg_restore cannot read is not a backup.
	if _, err := j.exec(ctx, pgRestore, "--list", tmp); err != nil {
		return "", fmt.Errorf("dump failed integrity check: %w", err)
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return "", err
	}
	j.set(func(rr *Run) { rr.File = file; rr.Size = st.Size() })
	j.logf("dump ok, %s; uploading to %s/", humanBytes(st.Size()), j.r.s3.remote(j.t.Prefix))

	remote := j.r.s3.remote(j.t.Prefix)
	if _, err := j.exec(ctx, "rclone", "copyto", tmp, remote+"/"+file, "--s3-no-check-bucket"); err != nil {
		return "", err
	}
	// Prune only files this target writes: <database>-*.dump.
	j.logf("pruning %s-*.dump older than %dd", j.t.Database, j.t.RetentionDays)
	if _, err := j.exec(ctx, "rclone", "delete", remote+"/", "--min-age", fmt.Sprintf("%dd", j.t.RetentionDays),
		"--include", j.t.Database+"-*.dump", "--s3-no-check-bucket"); err != nil {
		j.logf("prune failed (backup is still stored): %v", err)
	}
	j.logf("backup complete: %s", file)
	return file, nil
}

// fetch checks that file is one of the target's dumps and downloads it.
func (j *job) fetch(ctx context.Context, file string) (string, error) {
	files, err := j.r.List(ctx, j.t)
	if err != nil {
		return "", err
	}
	var found *RemoteFile
	for i := range files {
		if files[i].Name == file {
			found = &files[i]
		}
	}
	if found == nil {
		return "", fmt.Errorf("%s is not a backup of %s", file, j.t.Name)
	}
	if !safeName(file) {
		return "", fmt.Errorf("refusing unsafe file name %q", file)
	}
	tmp := filepath.Join(j.r.workdir, j.t.ID+"-restore-"+file)
	j.logf("downloading %s (%s)", file, humanBytes(found.Size))
	if _, err := j.exec(ctx, "rclone", "copyto", j.r.s3.remote(j.t.Prefix)+"/"+file, tmp, "--s3-no-check-bucket"); err != nil {
		return "", err
	}
	return tmp, nil
}

// restoreCopy restores a dump into a new database next to the live one.
// The live database is never touched. The new database is kept.
func (j *job) restoreCopy(file string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
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
	tmp, err := j.fetch(ctx, file)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	copyDB := fmt.Sprintf("%s_restore_%s", j.t.Database, time.Now().UTC().Format("20060102150405"))
	j.logf("creating database %s", copyDB)
	if _, err := j.exec(ctx, psql, append([]string{"-X", "-c", fmt.Sprintf("CREATE DATABASE %q", copyDB)}, j.connArgs(j.t.Database)...)...); err != nil {
		return err
	}
	j.logf("restoring %s into %s", file, copyDB)
	args := append([]string{"--no-owner", "--no-acl", "--exit-on-error"}, j.connArgs(copyDB)...)
	if _, err := j.exec(ctx, pgRestore, append(args, tmp)...); err != nil {
		return fmt.Errorf("%w (database %s is left for inspection)", err, copyDB)
	}
	j.logf("done. Live database untouched. Drop %s when you no longer need it.", copyDB)
	j.set(func(rr *Run) { rr.Message = "restored into " + copyDB })
	return nil
}

// restoreReplace makes the live database equal to a dump. It first takes a
// fresh backup, so the step can be undone. Then, in one psql transaction, it
// drops every user schema (so objects created after the dump go too) and
// replays the dump. Either all of it applies or nothing changes.
func (j *job) restoreReplace(file string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()

	j.logf("step 1/2: safety backup of the current live database")
	safety, err := j.dumpAndUpload(ctx)
	if err != nil {
		return fmt.Errorf("safety backup failed, live database untouched: %w", err)
	}
	// dumpAndUpload set File/Size to the safety dump; point the run back at the source.
	j.set(func(rr *Run) { rr.File = file; rr.Size = 0 })

	major, err := j.detect(ctx)
	if err != nil {
		return err
	}
	pgRestore, err := j.r.client(major, "pg_restore")
	if err != nil {
		return err
	}
	psql, _ := j.r.client(major, "psql")
	tmp, err := j.fetch(ctx, file)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	// Every user schema in the live database.
	out, err := j.exec(ctx, psql, append([]string{"-X", "-A", "-t", "-c",
		`SELECT quote_ident(nspname) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY 1`},
		j.connArgs(j.t.Database)...)...)
	if err != nil {
		return err
	}
	var pre strings.Builder
	for _, s := range strings.Fields(out) {
		fmt.Fprintf(&pre, "DROP SCHEMA %s CASCADE;\n", s)
	}
	// pg_dump does not emit CREATE SCHEMA public unless public was recreated
	// in the source; recreate it only when the archive will not.
	toc, err := j.exec(ctx, pgRestore, "--list", tmp)
	if err != nil {
		return err
	}
	if !strings.Contains(toc, " SCHEMA - public ") {
		pre.WriteString("CREATE SCHEMA public;\n")
	}
	j.logf("step 2/2: replacing %s with %s in one transaction (drop schemas: %s)", j.t.Database, file, strings.Join(strings.Fields(out), ", "))
	if err := j.replaceStream(ctx,
		[]string{pgRestore, "--no-owner", "--no-acl", "-f", "-", tmp},
		append([]string{psql, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-"}, j.connArgs(j.t.Database)...),
		pre.String(),
	); err != nil {
		return fmt.Errorf("%w (rolled back, live database unchanged; safety backup %s)", err, safety)
	}
	j.logf("done. To undo, replace again with %s", safety)
	j.set(func(rr *Run) { rr.Message = "replaced live; undo with " + safety })
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
	kept := files[:0]
	for _, f := range files {
		if safeName(f.Name) {
			kept = append(kept, f)
		}
	}
	files = kept
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
			_, _ = s.runner.Start(cur, KindBackup, "schedule", "")
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
