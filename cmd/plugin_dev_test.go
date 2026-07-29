package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeDevFile creates path (and its parent dirs) with content, failing the
// test on any error.
func writeDevFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveDevToken covers resolveDevToken's precedence: an explicit
// --token flag always wins; the ALIST_ADMIN_TOKEN env var is only consulted
// when the flag is empty; and both being empty/whitespace-only resolves to
// "" (the caller's required-flag error path).
func TestResolveDevToken(t *testing.T) {
	cases := []struct {
		name      string
		flagToken string
		env       string
		want      string
	}{
		{"flag set, env unset", "flag-token", "", "flag-token"},
		{"flag set, env also set: flag wins", "flag-token", "env-token", "flag-token"},
		{"flag empty, env set: falls back", "", "env-token", "env-token"},
		{"flag whitespace, env set: falls back", "   ", "env-token", "env-token"},
		{"both empty", "", "", ""},
		{"env whitespace only", "", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key != "ALIST_ADMIN_TOKEN" {
					t.Fatalf("resolveDevToken looked up unexpected env var %q", key)
				}
				return c.env
			}
			if got := resolveDevToken(c.flagToken, getenv); got != c.want {
				t.Errorf("resolveDevToken(%q, env=%q) = %q, want %q", c.flagToken, c.env, got, c.want)
			}
		})
	}
}

// TestPluginDevStageDir verifies stagePluginDir reproduces an install-package
// layout (plugin.json + i18n/ + a binary literally named "plugin") from a
// plugin source tree, dropping everything else (source files, VCS metadata).
func TestPluginDevStageDir(t *testing.T) {
	src := t.TempDir()
	writeDevFile(t, filepath.Join(src, "plugin.json"), `{"id":"test.plugin","version":"0.1.0"}`)
	writeDevFile(t, filepath.Join(src, "i18n", "en.json"), `{"greeting":"hi"}`)
	writeDevFile(t, filepath.Join(src, "main.go"), "package main\nfunc main() {}\n")
	writeDevFile(t, filepath.Join(src, ".git", "HEAD"), "ref: refs/heads/main\n")

	builtBin := filepath.Join(t.TempDir(), "built")
	writeDevFile(t, builtBin, "#!/bin/sh\necho hi\n")
	if err := os.Chmod(builtBin, 0o755); err != nil {
		t.Fatal(err)
	}

	staging, err := stagePluginDir(src, builtBin)
	if err != nil {
		t.Fatalf("stagePluginDir: %v", err)
	}
	defer os.RemoveAll(staging)

	manifest, err := os.ReadFile(filepath.Join(staging, "plugin.json"))
	if err != nil {
		t.Fatalf("staged plugin.json missing: %v", err)
	}
	if !strings.Contains(string(manifest), "test.plugin") {
		t.Fatalf("staged plugin.json content mismatch: %s", manifest)
	}

	locale, err := os.ReadFile(filepath.Join(staging, "i18n", "en.json"))
	if err != nil {
		t.Fatalf("staged i18n/en.json missing: %v", err)
	}
	if !strings.Contains(string(locale), "hi") {
		t.Fatalf("staged i18n content mismatch: %s", locale)
	}

	binName := "plugin"
	if runtime.GOOS == "windows" {
		binName = "plugin.exe"
	}
	info, err := os.Stat(filepath.Join(staging, binName))
	if err != nil {
		t.Fatalf("staged binary missing: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("staged binary not executable: mode=%v", info.Mode())
	}

	if _, err := os.Stat(filepath.Join(staging, "main.go")); !os.IsNotExist(err) {
		t.Fatalf("main.go must not be staged, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git must not be staged, stat err=%v", err)
	}

	// os.MkdirTemp defaults the staging dir to 0700 (owner-only); the CLI and
	// the alist server it pushes to are commonly different OS users, so
	// stagePluginDir must widen it to 0755 - a server running as a different
	// user must still be able to traverse into it (the staged contents are
	// public: manifest 0644, binary 0755, no secrets).
	if runtime.GOOS != "windows" { // Unix perm bits are not meaningful on windows
		dirInfo, err := os.Stat(staging)
		if err != nil {
			t.Fatalf("stat staging dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o755 {
			t.Fatalf("staging dir perm = %v, want 0755 (traversable by a different OS user)", perm)
		}
	}
}

// TestPluginDevSnapshotDetectsChange verifies snapshotTree/changed detect a
// real content change while ignoring dist/ and .git/ entirely.
func TestPluginDevSnapshotDetectsChange(t *testing.T) {
	src := t.TempDir()
	writeDevFile(t, filepath.Join(src, "a.txt"), "a")
	writeDevFile(t, filepath.Join(src, "dist", "out.bin"), "built")
	writeDevFile(t, filepath.Join(src, ".git", "HEAD"), "ref")

	snapA, err := snapshotTree(src)
	if err != nil {
		t.Fatalf("snapshotTree: %v", err)
	}
	snapB, err := snapshotTree(src)
	if err != nil {
		t.Fatalf("snapshotTree: %v", err)
	}
	if changed(snapA, snapB) {
		t.Fatalf("two immediate snapshots of an untouched tree must be equal")
	}

	// Changes under dist/ and .git/ must not be visible in the snapshot at
	// all (not merely "equal despite differing" - they're skipped subtrees).
	writeDevFile(t, filepath.Join(src, "dist", "out.bin"), "built-again-and-longer")
	writeDevFile(t, filepath.Join(src, ".git", "HEAD"), "ref-changed-and-longer")
	snapIgnored, err := snapshotTree(src)
	if err != nil {
		t.Fatalf("snapshotTree: %v", err)
	}
	if changed(snapA, snapIgnored) {
		t.Fatalf("changes under dist/ and .git/ must not be detected")
	}

	// a.txt's size changes, so this is detected regardless of filesystem
	// mtime resolution.
	writeDevFile(t, filepath.Join(src, "a.txt"), "a-changed")
	snapChanged, err := snapshotTree(src)
	if err != nil {
		t.Fatalf("snapshotTree: %v", err)
	}
	if !changed(snapA, snapChanged) {
		t.Fatalf("a.txt content/size change must be detected")
	}
}

// TestPluginDevPushOnce verifies pushDevLoad POSTs the dir to the dev/load
// endpoint with the raw token in the Authorization header, and that it
// distinguishes success from failure by the envelope's INNER code - both
// arrive over HTTP 200 (alist's error-envelope convention).
func TestPluginDevPushOnce(t *testing.T) {
	const token = "s3cr3t-admin-token"
	const dir = "/tmp/alist-plugin-dev-staging-xyz"

	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if !strings.HasSuffix(r.URL.Path, "/api/v4/admin/plugins/dev/load") {
				t.Errorf("path = %s, want suffix /api/v4/admin/plugins/dev/load", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != token {
				t.Errorf("Authorization header = %q, want %q (raw token, no Bearer prefix)", got, token)
			}
			var body struct {
				Dir string `json:"dir"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if body.Dir != dir {
				t.Errorf("body.dir = %q, want %q", body.Dir, dir)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"id":"test.plugin","version":"0.1.0","kinds":["storage_driver"],"reloaded_storages":2}}`))
		}))
		defer srv.Close()

		result, err := pushDevLoad(srv.URL, token, dir)
		if err != nil {
			t.Fatalf("pushDevLoad: %v", err)
		}
		if result == nil {
			t.Fatal("pushDevLoad: want a non-nil result on success")
		}
		if result.ID != "test.plugin" {
			t.Fatalf("result.ID = %q, want %q", result.ID, "test.plugin")
		}
		if result.Version != "0.1.0" {
			t.Fatalf("result.Version = %q, want %q", result.Version, "0.1.0")
		}
		if len(result.Kinds) != 1 || result.Kinds[0] != "storage_driver" {
			t.Fatalf("result.Kinds = %v, want [storage_driver]", result.Kinds)
		}
		if result.ReloadedStorages != 2 {
			t.Fatalf("result.ReloadedStorages = %d, want 2", result.ReloadedStorages)
		}
	})

	t.Run("inner error code over HTTP 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":500,"message":"x"}`))
		}))
		defer srv.Close()

		_, err := pushDevLoad(srv.URL, token, dir)
		if err == nil {
			t.Fatal("pushDevLoad: want error for inner code 500, got nil")
		}
		if !strings.Contains(err.Error(), "x") {
			t.Fatalf("pushDevLoad error = %v, want it to mention message %q", err, "x")
		}
	})

	t.Run("404 means the endpoint is not mounted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := pushDevLoad(srv.URL, token, dir)
		if err == nil {
			t.Fatal("pushDevLoad: want error for HTTP 404, got nil")
		}
		if !strings.Contains(err.Error(), "--dev") {
			t.Fatalf("pushDevLoad 404 error should explain the server needs --dev, got: %v", err)
		}
	})
}

// TestPluginDevBuildFailureKeepsWatching verifies runPluginDevOnce surfaces a
// build failure as a plain error - not a panic - so the caller's watch loop
// can log it and keep polling instead of dying. The build command runs
// through `sh -c`, so plain "false" (a POSIX shell builtin/utility available
// everywhere) reproduces a failing build without depending on /bin/false
// existing as a file (it doesn't on macOS).
// TestBuildCommandUnix pins buildOnce's shell selection on every unix-like
// GOOS: `sh -c <resolved script>` inline, no temp file - "{out}" substituted
// into the script before sh sees it.
func TestBuildCommandUnix(t *testing.T) {
	cases := []struct {
		goos     string
		script   string
		out      string
		wantArgs []string
	}{
		{"linux", "go build -o {out} .", "/tmp/out", []string{"-c", "go build -o /tmp/out ."}},
		{"darwin", "go build -o {out} .", "/tmp/out", []string{"-c", "go build -o /tmp/out ."}},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			name, args, cleanup, err := buildCommand(c.goos, c.script, c.out)
			if err != nil {
				t.Fatalf("buildCommand(%q): unexpected error: %v", c.goos, err)
			}
			defer cleanup()
			if name != "sh" {
				t.Errorf("buildCommand(%q): name = %q, want sh", c.goos, name)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("buildCommand(%q): args = %v, want %v", c.goos, args, c.wantArgs)
			}
			for i := range args {
				if args[i] != c.wantArgs[i] {
					t.Errorf("buildCommand(%q): args[%d] = %q, want %q", c.goos, i, args[i], c.wantArgs[i])
				}
			}
		})
	}
}

// TestBuildCommandWindowsWritesTempScript pins the quoting fix: on windows,
// buildCommand must NOT pass the resolved script inline as a `cmd /c`
// argument - Go's argv escaping and cmd.exe's quote-stripping compose badly,
// mangling a script containing double quotes (the common case, e.g. a
// -ldflags value). Instead it writes the resolved script verbatim to a temp
// .cmd file and returns `cmd /c <path>`, so any script content - including
// embedded double quotes - survives untouched. This runs on every host OS
// (it is a pure function over the goos parameter, not a real windows box);
// only the temp-file plumbing is exercised, never cmd.exe itself.
func TestBuildCommandWindowsWritesTempScript(t *testing.T) {
	script := `go build -ldflags="-X main.version=1.0 -X 'main.date=2026-07-27'" -o {out} .`
	out := `C:\out\plugin.exe`
	wantContent := `go build -ldflags="-X main.version=1.0 -X 'main.date=2026-07-27'" -o C:\out\plugin.exe .`

	name, args, cleanup, err := buildCommand("windows", script, out)
	if err != nil {
		t.Fatalf("buildCommand(windows): unexpected error: %v", err)
	}
	if name != "cmd" {
		t.Fatalf("buildCommand(windows): name = %q, want cmd", name)
	}
	if len(args) != 2 || args[0] != "/c" {
		t.Fatalf("buildCommand(windows): args = %v, want [/c <tempfile>]", args)
	}
	path := args[1]

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("temp build script missing at %q: %v", path, err)
	}
	if string(got) != wantContent {
		t.Fatalf("temp build script content = %q, want %q (quotes must survive verbatim)", got, wantContent)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the temp build script, stat err=%v", err)
	}
}

func TestPluginDevBuildFailureKeepsWatching(t *testing.T) {
	src := t.TempDir()
	writeDevFile(t, filepath.Join(src, "plugin.json"), `{"id":"test.plugin","version":"0.1.0"}`)

	opts := pluginDevOptions{
		Dir:      src,
		Server:   "http://127.0.0.1:0",
		Token:    "unused",
		BuildCmd: "false",
		Interval: 700 * time.Millisecond,
	}

	_, err := runPluginDevOnce(opts, io.Discard)
	if err == nil {
		t.Fatal("runPluginDevOnce: want error when the build command fails, got nil")
	}
}

// devLoadTestServer starts an httptest server standing in for the dev/load
// endpoint, always answering 200 + a success envelope. Used by the staging
// lifecycle tests below, which care about what happens to the staging dir on
// the CLI side, not about exercising the real handler.
func devLoadTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"id":"test.plugin","version":"0.1.0","reloaded_storages":0}}`))
	}))
}

// TestPluginDevOnceKeepsStagingAliveOnSuccess is the C1 regression: the
// server stores the staged directory's path as its registry Entry.binPath
// and keeps exec'ing it long after this request completes (lazy Acquire,
// crash Restart) - so a successful runPluginDevOnce must hand the staging
// dir BACK to its caller still on disk, not delete it out from under the
// server the moment the push completes.
func TestPluginDevOnceKeepsStagingAliveOnSuccess(t *testing.T) {
	srv := devLoadTestServer(t)
	defer srv.Close()

	src := t.TempDir()
	writeDevFile(t, filepath.Join(src, "plugin.json"), `{"id":"test.plugin","version":"0.1.0"}`)

	opts := pluginDevOptions{
		Dir:      src,
		Server:   srv.URL,
		Token:    "unused",
		BuildCmd: "printf '#!/bin/sh\\necho hi\\n' > {out} && chmod +x {out}",
		Interval: 700 * time.Millisecond,
	}

	staging, err := runPluginDevOnce(opts, io.Discard)
	if err != nil {
		t.Fatalf("runPluginDevOnce: %v", err)
	}
	defer os.RemoveAll(staging)

	if staging == "" {
		t.Fatal("runPluginDevOnce: want a non-empty staging path on success")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staging dir must still exist after a successful push (server keeps using this path as binPath): %v", err)
	}
}

// TestPluginDevOnceRemovesStagingOnRejectedPush verifies the opposite side of
// the same invariant: if the server REJECTS the push (never adopts the
// path as its binPath), runPluginDevOnce must not leak the staging dir - it
// cleans it up itself and returns "" alongside the error.
func TestPluginDevOnceRemovesStagingOnRejectedPush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":500,"message":"rejected"}`))
	}))
	defer srv.Close()

	src := t.TempDir()
	writeDevFile(t, filepath.Join(src, "plugin.json"), `{"id":"test.plugin","version":"0.1.0"}`)

	opts := pluginDevOptions{
		Dir:      src,
		Server:   srv.URL,
		Token:    "unused",
		BuildCmd: "printf '#!/bin/sh\\necho hi\\n' > {out} && chmod +x {out}",
		Interval: 700 * time.Millisecond,
	}

	staging, err := runPluginDevOnce(opts, io.Discard)
	if err == nil {
		t.Fatal("runPluginDevOnce: want error when the server rejects the push")
	}
	if staging != "" {
		os.RemoveAll(staging)
		t.Fatalf("runPluginDevOnce: want empty staging path on a rejected push, got %q", staging)
	}
}

// TestPluginDevStagingRotation is the direct unit test of pluginDevStaging:
// the first successfully-pushed staging dir must survive its own rotate
// (nothing to reclaim yet), the SECOND successful rotate must reclaim the
// first now that the server has moved on to the second, and cleanup must
// reclaim whatever is left when the watch loop exits.
func TestPluginDevStagingRotation(t *testing.T) {
	dir1, err := os.MkdirTemp("", "alist-plugin-dev-rotation-*")
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := os.MkdirTemp("", "alist-plugin-dev-rotation-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(dir1)
		os.RemoveAll(dir2)
	})

	var s pluginDevStaging

	s.rotate(dir1)
	if _, err := os.Stat(dir1); err != nil {
		t.Fatalf("first successfully-pushed staging dir must survive its own rotate: %v", err)
	}

	s.rotate(dir2)
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Fatalf("previous round's staging dir must be reclaimed once the NEXT push succeeds, stat err=%v", err)
	}
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("newly active staging dir must survive: %v", err)
	}

	// A failed round (empty path) must never touch the still-live active dir.
	s.rotate("")
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("rotate(\"\") for a failed round must not touch the active staging dir: %v", err)
	}

	s.cleanup()
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatalf("cleanup must reclaim the last active staging dir on exit, stat err=%v", err)
	}
}
