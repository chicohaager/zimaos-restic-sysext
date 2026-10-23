package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The handler tests run the real restic (RESTIC_BIN: path of the binary, set
// by build.sh to the downloaded release) and the real restic-zima script
// against a repository in a temp directory; systemctl is recorded, not run.
// Without RESTIC_BIN they are skipped, the pure tests still run.
var script, _ = filepath.Abs("../../files/usr/bin/restic-zima")

type world struct {
	calls [][]string // systemctl and systemd-run calls
	src   string     // a directory with files to back up
}

func realRun(t *testing.T) *world {
	t.Helper()
	bin := os.Getenv("RESTIC_BIN")
	if bin == "" {
		t.Skip("RESTIC_BIN not set")
	}
	baseDir, unitDir, restoreDir = t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "restore")
	w := &world{src: t.TempDir()}
	for f, text := range map[string]string{"a.txt": "eins\n", "scratch.tmp": "weg\n"} {
		if err := os.WriteFile(filepath.Join(w.src, f), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "RESTIC_ZIMA_DIR="+baseDir, "RESTIC_ZIMA_UNIT_DIR="+unitDir,
		"RESTIC_ZIMA_SYSTEMCTL=true", "RESTIC_ZIMA_RESTIC="+bin)
	shell := func(ctx context.Context, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
		c.Env = env
		return c.CombinedOutput()
	}
	run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "restic-zima":
			return shell(ctx, args...)
		case "systemctl":
			w.calls = append(w.calls, append([]string{name}, args...))
			return []byte("inactive\n"), nil
		case "systemd-run": // run the command in place, synchronously
			w.calls = append(w.calls, append([]string{name}, args...))
			for i, a := range args {
				if a == "/usr/bin/restic-zima" {
					return shell(ctx, args[i+1:]...)
				}
			}
			t.Fatalf("systemd-run without restic-zima: %v", args)
		}
		t.Fatalf("unexpected command %s %v", name, args)
		return nil, nil
	}
	return w
}

func call(t *testing.T, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	w := httptest.NewRecorder()
	api(func(h http.Handler) http.Handler { return h }).ServeHTTP(w, httptest.NewRequest(method, path, &buf))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (w *world) job(name, repo string, extra map[string]any) map[string]any {
	j := map[string]any{"name": name, "repository": repo, "paths": []string{w.src}, "init": true}
	for k, v := range extra {
		j[k] = v
	}
	return j
}

func TestValidRepository(t *testing.T) {
	for _, ok := range []string{"/media/Backup/restic", "/DATA/restic", "rest:http://192.0.2.10:8000/zima", "rest:https://u:p@backup.example/x", "sftp:user@host:/srv/restic", "s3:s3.amazonaws.com/bucket"} {
		if !validRepository(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "relative/path", "/a/../b", "/a//b", "rest:http://a b", "ftp:host/x", "/a\n/b", "rest:"} {
		if validRepository(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMaskRepository(t *testing.T) {
	cases := map[string]string{
		"rest:https://backup:geheim@backup.example:8000/zima": "rest:https://backup:***@backup.example:8000/zima",
		"rest:http://192.0.2.10:8000/zima":                   "rest:http://192.0.2.10:8000/zima",
		"/media/Backup/restic":                               "/media/Backup/restic",
		"sftp:user@host:/srv/restic":                         "sftp:user@host:/srv/restic",
	}
	for in, want := range cases {
		if got := maskRepository(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(maskRepository(in), "geheim") {
			t.Errorf("password left in %q", maskRepository(in))
		}
	}
}

// The whole way: create (repository made, password generated), back up as the
// timer does, see the run and the snapshot in the tile, restore it elsewhere.
func TestBackupAndRestore(t *testing.T) {
	w := realRun(t)
	repo := filepath.Join(t.TempDir(), "repo")
	res, out := call(t, "POST", "/restic/jobs", w.job("daten", repo, map[string]any{"schedule": "03:15", "excludes": []string{"*.tmp"}}))
	if res.Code != 200 || out["status"] != "created" {
		t.Fatalf("create: %d %v", res.Code, out)
	}
	pw, _ := out["generated_password"].(string)
	if len(pw) < 30 {
		t.Fatalf("generated password %q", pw)
	}
	for _, f := range []string{"repository", "password", "paths"} {
		st, err := os.Stat(filepath.Join(jobDir("daten"), f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(unitDir, "restic-zima-daten.timer")); !strings.Contains(string(b), "OnCalendar=*-*-* 03:15:00") {
		t.Fatalf("timer: %s", b)
	}

	if o, err := run(context.Background(), "restic-zima", "run", "daten"); err != nil {
		t.Fatalf("run: %v %s", err, o)
	}
	_, out = call(t, "GET", "/restic/jobs", nil)
	jobs := out["jobs"].([]any)
	j := jobs[0].(map[string]any)
	last := j["last"].(map[string]any)
	if last["rc"].(float64) != 0 || last["summary"].(map[string]any)["files_new"].(float64) != 1 {
		t.Fatalf("last run: %v", last)
	}
	if !j["timer"].(bool) || j["repository"] != repo {
		t.Fatalf("job: %v", j)
	}

	res, out = call(t, "GET", "/restic/snapshots?name=daten", nil)
	snaps := out["snapshots"].([]any)
	if res.Code != 200 || len(snaps) != 1 {
		t.Fatalf("snapshots: %d %v", res.Code, out)
	}
	id := snaps[0].(map[string]any)["id"].(string)

	res, out = call(t, "POST", "/restic/restore", map[string]any{"name": "daten", "snapshot": id})
	if res.Code != 200 {
		t.Fatalf("restore: %d %v", res.Code, out)
	}
	target := out["target"].(string)
	if !strings.HasPrefix(target, restoreDir+"/daten-") {
		t.Fatalf("target %s outside %s", target, restoreDir)
	}
	b, err := os.ReadFile(filepath.Join(target, w.src, "a.txt"))
	if err != nil || string(b) != "eins\n" {
		t.Fatalf("restored file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(target, w.src, "scratch.tmp")); err == nil {
		t.Fatal("excluded *.tmp was backed up")
	}
	_, out = call(t, "GET", "/restic/jobs", nil)
	rs := out["jobs"].([]any)[0].(map[string]any)["restore"].(map[string]any)
	if rs["rc"].(float64) != 0 || rs["target"] != target {
		t.Fatalf("restore.json: %v", rs)
	}

	_, out = call(t, "POST", "/restic/jobs/password", map[string]any{"name": "daten"})
	if out["password"] != pw {
		t.Fatal("password endpoint does not return the generated password")
	}
}

// restic exits 0 when a path is gone (measured 0.19.1); restic-zima must not.
func TestMissingPathIsAWarning(t *testing.T) {
	w := realRun(t)
	gone := filepath.Join(t.TempDir(), "usb")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	job := w.job("zwei", filepath.Join(t.TempDir(), "repo"), map[string]any{"paths": []string{w.src, gone}})
	if res, out := call(t, "POST", "/restic/jobs", job); res.Code != 200 {
		t.Fatalf("create: %d %v", res.Code, out)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	_, err := run(context.Background(), "restic-zima", "run", "zwei")
	var ee *exec.ExitError
	if err == nil || !errorsAs(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("run with a missing path: %v, want exit 3", err)
	}
	var last struct {
		RC           int    `json:"rc"`
		Missing      int    `json:"missing_paths"`
		FirstMissing string `json:"first_missing"`
	}
	b, _ := os.ReadFile(filepath.Join(jobDir("zwei"), "last.json"))
	if err := json.Unmarshal(b, &last); err != nil || last.RC != 0 || last.Missing != 1 || last.FirstMissing != gone {
		t.Fatalf("last.json: %s %v", b, err)
	}
}

func TestCreateRefusals(t *testing.T) {
	w := realRun(t)
	repo := filepath.Join(t.TempDir(), "repo")
	res, out := call(t, "POST", "/restic/jobs", w.job("leer", repo, map[string]any{"init": false}))
	if res.Code != 400 || out["code"] != "no_repository" {
		t.Fatalf("no repository, no init: %d %v", res.Code, out)
	}
	if _, err := os.Stat(jobDir("leer")); err == nil {
		t.Fatal("job dir left behind after a refused create")
	}
	if res, out := call(t, "POST", "/restic/jobs", w.job("eins", repo, map[string]any{"password": "richtig"})); res.Code != 200 {
		t.Fatalf("create: %d %v", res.Code, out)
	}
	res, out = call(t, "POST", "/restic/jobs", w.job("zwei", repo, map[string]any{"password": "falsch"}))
	if res.Code != 400 || out["code"] != "wrong_password" {
		t.Fatalf("wrong password: %d %v", res.Code, out)
	}
	res, out = call(t, "POST", "/restic/jobs", w.job("eins", repo, map[string]any{"password": "richtig"}))
	if res.Code != 409 {
		t.Fatalf("same name twice: %d %v", res.Code, out)
	}
	for name, body := range map[string]map[string]any{
		"missing_path": w.job("x1", repo, map[string]any{"paths": []string{"/gibt/es/nicht"}}),
		"bad_path":     w.job("x2", repo, map[string]any{"paths": []string{"relativ"}}),
		"bad_schedule": w.job("x3", repo, map[string]any{"schedule": "25:00"}),
		"bad_name":     w.job("../x", repo, nil),
		"no_paths":     w.job("x4", repo, map[string]any{"paths": []string{" "}}),
	} {
		if res, out := call(t, "POST", "/restic/jobs", body); res.Code != 400 || out["code"] != name {
			t.Errorf("%s: %d %v", name, res.Code, out)
		}
	}
}

func TestScheduleAndDelete(t *testing.T) {
	w := realRun(t)
	if res, out := call(t, "POST", "/restic/jobs", w.job("plan", filepath.Join(t.TempDir(), "repo"), map[string]any{"schedule": "02:00"})); res.Code != 200 {
		t.Fatalf("create: %d %v", res.Code, out)
	}
	timer := filepath.Join(unitDir, "restic-zima-plan.timer")
	service := filepath.Join(unitDir, "restic-zima-plan.service")
	if res, out := call(t, "POST", "/restic/jobs/schedule", map[string]any{"name": "plan", "schedule": ""}); res.Code != 200 {
		t.Fatalf("unschedule: %d %v", res.Code, out)
	}
	if _, err := os.Stat(timer); err == nil {
		t.Fatal("timer still there after clearing the schedule")
	}
	if b, _ := os.ReadFile(service); !strings.Contains(string(b), "ExecStart=/usr/bin/restic-zima run plan") {
		t.Fatalf("service: %s", b)
	}
	if res, out := call(t, "POST", "/restic/jobs/run", map[string]any{"name": "plan"}); res.Code != 200 {
		t.Fatalf("run now: %d %v", res.Code, out)
	}
	last := w.calls[len(w.calls)-1]
	if strings.Join(last, " ") != "systemctl start --no-block restic-zima-plan.service" {
		t.Fatalf("run now called %v", last)
	}
	res, out := call(t, "POST", "/restic/jobs/delete", map[string]any{"name": "plan"})
	if res.Code != 200 {
		t.Fatalf("delete: %d %v", res.Code, out)
	}
	if _, err := os.Stat(service); err == nil {
		t.Fatal("service unit left after delete")
	}
	if _, err := os.Stat(filepath.Join(out["kept_in"].(string), "password")); err != nil {
		t.Fatalf("password not kept after delete: %v", err)
	}
	if res, _ := call(t, "POST", "/restic/jobs/run", map[string]any{"name": "plan"}); res.Code != 404 {
		t.Fatalf("deleted job still runs: %d", res.Code)
	}
}

func TestRestoreRefusesAnExistingTarget(t *testing.T) {
	w := realRun(t)
	if res, out := call(t, "POST", "/restic/jobs", w.job("ziel", filepath.Join(t.TempDir(), "repo"), nil)); res.Code != 200 {
		t.Fatalf("create: %d %v", res.Code, out)
	}
	exists := t.TempDir()
	o, err := run(context.Background(), "restic-zima", "restore", "ziel", "abcdef12", exists)
	if err == nil || !strings.Contains(string(o), "restore only into a new directory") {
		t.Fatalf("restore into an existing dir: %v %s", err, o)
	}
}

func errorsAs(err error, target **exec.ExitError) bool { return errors.As(err, target) }
