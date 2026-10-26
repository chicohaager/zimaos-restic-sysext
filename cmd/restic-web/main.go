// Command restic-web serves the restic tile: backup jobs (repository,
// password, paths, a daily time), run now, the repository's snapshots and
// restoring one into a new directory. The work itself is done by restic-zima
// and systemd; this backend writes the job files and reads their results.
// It never forgets or prunes snapshots.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chicohaager/lintux-modkit/httpx"
	"github.com/chicohaager/lintux-modkit/webmod"
)

// version is set at build time from VERSION (-ldflags -X main.version=...).
var version = "dev"

// Paths and commands; tests point them elsewhere.
var (
	baseDir    = "/DATA/AppData/restic"
	unitDir    = "/etc/systemd/system"
	restoreDir = "/DATA/restic-restore"
	run        = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

var mu sync.Mutex

func main() {
	webmod.Run(options())
}

func options() webmod.Options {
	return webmod.Options{
		Name: "restic", Service: "restic-web", Binary: "/usr/bin/restic-web",
		Route: "/restic", Version: version, API: api,
		// 0777 without sticky bit on ZimaOS 1.8: checked before the tile reads the jobs (KB §79.11)
		DataDirs: []string{baseDir},
	}
}

func api(verify func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/restic/jobs", verify(http.HandlerFunc(jobsHandler)))
	mux.Handle("/restic/jobs/run", verify(post(runHandler)))
	mux.Handle("/restic/jobs/schedule", verify(post(scheduleHandler)))
	mux.Handle("/restic/jobs/delete", verify(post(deleteHandler)))
	mux.Handle("/restic/jobs/password", verify(post(passwordHandler)))
	mux.Handle("/restic/snapshots", verify(http.HandlerFunc(snapshotsHandler)))
	mux.Handle("/restic/restore", verify(post(restoreHandler)))
	return mux
}

func post(h func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpx.MethodNotAllowed(w)
			return
		}
		h(w, r)
	})
}

func jobDir(name string) string { return filepath.Join(baseDir, "jobs", name) }

func ctxFor(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func writeFile(path, text string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func firstLine(path string) string {
	l := readLines(path)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

// restic runs restic with a job's repository and password, through
// restic-zima so the cache directory and flags are the same as the timer's.
func restic(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	out, err := run(ctx, "restic-zima", append([]string{"restic", name}, args...)...)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), err
	}
	return out, 0, err
}

// ---------- validation ----------

var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	timeRe     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	snapRe     = regexp.MustCompile(`^[a-f0-9]{8,64}$`)
	backendRe  = regexp.MustCompile(`^(rest|sftp|s3|b2|azure|gs|swift|rclone):\S+$`)
	controlsRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// validRepository accepts an absolute local path or a restic backend URL, on
// one line (it is written to a file restic reads with --repository-file).
func validRepository(s string) bool {
	if s == "" || controlsRe.MatchString(s) {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return filepath.Clean(s) == strings.TrimRight(s, "/") || s == "/"
	}
	return backendRe.MatchString(s)
}

// maskRepository hides a password in the URL of a rest server
// (rest:https://user:secret@host/repo -> rest:https://user:***@host/repo).
func maskRepository(s string) string {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || !strings.Contains(rest, "@") || !(strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://")) {
		return s
	}
	u, err := url.Parse(rest)
	if err != nil || u.User == nil {
		return scheme + ":***"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return scheme + ":" + strings.Replace(u.String(), "%2A%2A%2A", "***", 1)
}

func checkPaths(lines []string, what string) error {
	for _, p := range lines {
		if !strings.HasPrefix(p, "/") || controlsRe.MatchString(p) {
			return fmt.Errorf("%s: %q is not an absolute path", what, p)
		}
	}
	return nil
}

func newPassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- jobs ----------

// Job is what GET /restic/jobs reports for one job.
type Job struct {
	Name       string          `json:"name"`
	Repository string          `json:"repository"`
	Paths      []string        `json:"paths"`
	Excludes   []string        `json:"excludes"`
	Schedule   string          `json:"schedule"`
	Timer      bool            `json:"timer"`
	Next       string          `json:"next,omitempty"`
	State      string          `json:"state"`
	Last       json.RawMessage `json:"last,omitempty"`
	Restore    json.RawMessage `json:"restore,omitempty"`
}

func jobsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listJobs(w, r)
	case http.MethodPost:
		createJob(w, r)
	default:
		httpx.MethodNotAllowed(w)
	}
}

func listJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	entries, _ := os.ReadDir(filepath.Join(baseDir, "jobs"))
	jobs := []Job{}
	for _, e := range entries {
		d := jobDir(e.Name())
		if !e.IsDir() || !nameRe.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "repository")); err != nil {
			continue
		}
		j := Job{
			Name: e.Name(), Repository: maskRepository(firstLine(filepath.Join(d, "repository"))),
			Paths: readLines(filepath.Join(d, "paths")), Excludes: readLines(filepath.Join(d, "excludes")),
			Schedule: firstLine(filepath.Join(d, "schedule")),
		}
		unit := "restic-zima-" + e.Name()
		if _, err := os.Stat(filepath.Join(unitDir, unit+".timer")); err == nil {
			j.Timer = true
			if out, err := run(ctx, "systemctl", "show", "-p", "NextElapseUSecRealtime", "--value", unit+".timer"); err == nil {
				j.Next = strings.TrimSpace(string(out))
			}
		}
		a, _ := run(ctx, "systemctl", "is-active", unit+".service")
		j.State = strings.TrimSpace(string(a))
		if b, err := os.ReadFile(filepath.Join(d, "last.json")); err == nil && json.Valid(b) {
			j.Last = b
		}
		if b, err := os.ReadFile(filepath.Join(d, "restore.json")); err == nil && json.Valid(b) {
			j.Restore = b
		}
		jobs = append(jobs, j)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"base_dir": baseDir, "restore_dir": restoreDir, "jobs": jobs})
}

func createJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string   `json:"name"`
		Repository string   `json:"repository"`
		Password   string   `json:"password"` // empty: generate one
		Paths      []string `json:"paths"`
		Excludes   []string `json:"excludes"`
		Schedule   string   `json:"schedule"` // HH:MM or empty
		Init       bool     `json:"init"`     // create the repository if it does not exist
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Repository = strings.TrimSpace(in.Repository)
	paths, excludes := clean(in.Paths), clean(in.Excludes)
	switch {
	case !nameRe.MatchString(in.Name):
		httpx.WriteError(w, http.StatusBadRequest, "bad_name", "name: up to 40 letters, digits, _ and -")
		return
	case !validRepository(in.Repository):
		httpx.WriteError(w, http.StatusBadRequest, "bad_repository", "repository: an absolute path (e.g. /media/Backup/restic) or a restic URL (rest:http://host:8000/name, sftp:user@host:/path …)")
		return
	case in.Schedule != "" && !timeRe.MatchString(in.Schedule):
		httpx.WriteError(w, http.StatusBadRequest, "bad_schedule", "time: HH:MM, or empty to run by hand")
		return
	case strings.ContainsAny(in.Password, "\n\r"):
		httpx.WriteError(w, http.StatusBadRequest, "bad_password", "the password must be one line")
		return
	case len(paths) == 0:
		httpx.WriteError(w, http.StatusBadRequest, "no_paths", "at least one path to back up")
		return
	}
	if err := checkPaths(paths, "path"); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	for _, e := range excludes { // restic patterns: "*.tmp" is as valid as "/DATA/x/cache"
		if controlsRe.MatchString(e) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_exclude", "an exclude pattern must be one line")
			return
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "missing_path", fmt.Sprintf("%s does not exist on this host", p))
			return
		}
	}
	mu.Lock()
	defer mu.Unlock()
	d := jobDir(in.Name)
	if _, err := os.Stat(d); err == nil {
		httpx.WriteError(w, http.StatusConflict, "exists", "a job with this name exists")
		return
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		httpx.WriteErr(w, err)
		return
	}
	generated := ""
	if in.Password == "" {
		generated = newPassword()
		in.Password = generated
	}
	files := map[string]string{
		"repository": in.Repository + "\n", "password": in.Password + "\n",
		"paths": strings.Join(paths, "\n") + "\n", "excludes": strings.Join(excludes, "\n"), "schedule": in.Schedule,
	}
	for f, text := range files {
		if err := writeFile(filepath.Join(d, f), text); err != nil {
			_ = os.RemoveAll(d)
			httpx.WriteErr(w, err)
			return
		}
	}
	ctx, cancel := ctxFor(r, 120*time.Second)
	defer cancel()
	// Exit codes of restic 0.19.1 (measured): 10 = no repository there, 12 = wrong password.
	status := "connected"
	if out, code, err := restic(ctx, in.Name, "cat", "config"); err != nil {
		switch {
		case code == 10 && in.Init:
			if out, _, err := restic(ctx, in.Name, "init"); err != nil {
				_ = os.RemoveAll(d)
				httpx.WriteError(w, http.StatusBadGateway, "init_failed", tail(out))
				return
			}
			status = "created"
		case code == 10:
			_ = os.RemoveAll(d)
			httpx.WriteError(w, http.StatusBadRequest, "no_repository", "there is no restic repository at this location — tick “create it” to make one")
			return
		case code == 12:
			_ = os.RemoveAll(d)
			httpx.WriteError(w, http.StatusBadRequest, "wrong_password", "the repository exists, but the password does not open it")
			return
		default:
			_ = os.RemoveAll(d)
			httpx.WriteError(w, http.StatusBadGateway, "repository", tail(out))
			return
		}
	}
	if out, err := run(ctx, "restic-zima", "units", in.Name); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "units", "the job is saved, but its units failed: "+tail(out))
		return
	}
	resp := map[string]string{"status": status}
	if generated != "" {
		resp["generated_password"] = generated
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func clean(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tail is the last non-empty line of a command's output, for error messages.
func tail(out []byte) string {
	l := clean(strings.Split(string(out), "\n"))
	if len(l) == 0 {
		return "no output"
	}
	return l[len(l)-1]
}

// named decodes {"name": …} plus extra fields into v and checks the job exists.
func named(w http.ResponseWriter, r *http.Request, v interface{ jobName() string }) bool {
	if !httpx.Decode(w, r, v) {
		return false
	}
	n := v.jobName()
	if !nameRe.MatchString(n) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_name", "unknown job")
		return false
	}
	if _, err := os.Stat(filepath.Join(jobDir(n), "repository")); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "no_job", "no job "+n)
		return false
	}
	return true
}

type nameOnly struct {
	Name string `json:"name"`
}

func (n *nameOnly) jobName() string { return n.Name }

func runHandler(w http.ResponseWriter, r *http.Request) {
	var in nameOnly
	if !named(w, r, &in) {
		return
	}
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	if out, err := run(ctx, "systemctl", "start", "--no-block", "restic-zima-"+in.Name+".service"); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "start_failed", tail(out))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

type scheduleIn struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
}

func (s *scheduleIn) jobName() string { return s.Name }

func scheduleHandler(w http.ResponseWriter, r *http.Request) {
	var in scheduleIn
	if !named(w, r, &in) {
		return
	}
	if in.Schedule != "" && !timeRe.MatchString(in.Schedule) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_schedule", "time: HH:MM, or empty to run by hand")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := writeFile(filepath.Join(jobDir(in.Name), "schedule"), in.Schedule); err != nil {
		httpx.WriteErr(w, err)
		return
	}
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	if out, err := run(ctx, "restic-zima", "units", in.Name); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "units", tail(out))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// deleteHandler removes the job's units and moves its files aside — they hold
// the password, without which the repository cannot be read. The repository
// itself is not touched.
func deleteHandler(w http.ResponseWriter, r *http.Request) {
	var in nameOnly
	if !named(w, r, &in) {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	if out, err := run(ctx, "restic-zima", "remove", in.Name); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "remove_failed", tail(out))
		return
	}
	dst := filepath.Join(baseDir, "jobs", ".deleted", in.Name+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		httpx.WriteErr(w, err)
		return
	}
	if err := os.Rename(jobDir(in.Name), dst); err != nil {
		httpx.WriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted", "kept_in": dst})
}

func passwordHandler(w http.ResponseWriter, r *http.Request) {
	var in nameOnly
	if !named(w, r, &in) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"password": firstLine(filepath.Join(jobDir(in.Name), "password"))})
}

// Snapshot is the part of `restic snapshots --json` the tile shows.
type Snapshot struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"short_id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Paths    []string `json:"paths"`
	Tags     []string `json:"tags"`
}

func snapshotsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w)
		return
	}
	n := r.URL.Query().Get("name")
	if !nameRe.MatchString(n) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_name", "unknown job")
		return
	}
	if _, err := os.Stat(filepath.Join(jobDir(n), "repository")); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "no_job", "no job "+n)
		return
	}
	ctx, cancel := ctxFor(r, 120*time.Second)
	defer cancel()
	out, _, err := restic(ctx, n, "snapshots", "--json", "--no-lock")
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "snapshots", tail(out))
		return
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "snapshots", "unreadable output of restic snapshots")
		return
	}
	if snaps == nil {
		snaps = []Snapshot{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

type restoreIn struct {
	Name     string `json:"name"`
	Snapshot string `json:"snapshot"`
}

func (s *restoreIn) jobName() string { return s.Name }

// restoreHandler restores a snapshot into a new directory under restoreDir,
// in a transient unit so it outlives the request; never over the originals.
func restoreHandler(w http.ResponseWriter, r *http.Request) {
	var in restoreIn
	if !named(w, r, &in) {
		return
	}
	if !snapRe.MatchString(in.Snapshot) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_snapshot", "snapshot: its id")
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	target := filepath.Join(restoreDir, in.Name+"-"+in.Snapshot[:8]+"-"+stamp)
	ctx, cancel := ctxFor(r, 30*time.Second)
	defer cancel()
	unit := "restic-zima-restore-" + in.Name + "-" + stamp
	if out, err := run(ctx, "systemd-run", "--unit="+unit, "--no-block", "--property=After=systemd-sysext.service",
		"/usr/bin/restic-zima", "restore", in.Name, in.Snapshot, target); err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "restore_failed", tail(out))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "started", "target": target, "unit": unit})
}
