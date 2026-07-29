package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// pluginDevOptions are the resolved `alist plugin dev` inputs, independent of
// cobra flag parsing so the build/stage/push pipeline (runPluginDevOnce) is
// directly unit-testable.
type pluginDevOptions struct {
	Dir, Server, Token, BuildCmd string
	Interval                     time.Duration
}

var pluginDevCmd = &cobra.Command{
	Use:   "dev --dir <plugin-src-dir> --server <url> --token <admin-token> [--build \"go build -o {out} .\"] [--interval 700ms]",
	Short: "Watch a plugin source dir, rebuild it, and hot-push it into a --dev alist server",
	Long: `Watch a plugin source dir, rebuild it, and hot-push it into a running alist
server via the dev-only POST /api/v4/admin/plugins/dev/load endpoint.

That endpoint only exists when the server itself is started with --dev - it
executes an unsigned local binary path with no signature check, so it is
never mounted otherwise.`,
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		server, _ := cmd.Flags().GetString("server")
		token, _ := cmd.Flags().GetString("token")
		buildCmd, _ := cmd.Flags().GetString("build")
		interval, _ := cmd.Flags().GetDuration("interval")

		token = resolveDevToken(token, os.Getenv)
		if token == "" {
			fmt.Fprintln(os.Stderr, "plugin dev: --token is required (copy an admin token from the admin panel, or set $ALIST_ADMIN_TOKEN)")
			os.Exit(1)
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plugin dev:", err)
			os.Exit(1)
		}

		opts := pluginDevOptions{
			Dir:      absDir,
			Server:   server,
			Token:    token,
			BuildCmd: buildCmd,
			Interval: interval,
		}

		fmt.Printf("plugin dev: watching %s, pushing to %s (the server must be running with --dev)\n", opts.Dir, opts.Server)
		fmt.Println("plugin dev: press Ctrl+C to stop")

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		runPluginDev(ctx, opts, os.Stdout)
		fmt.Println("plugin dev: stopped")
	},
}

func init() {
	pluginCmd.AddCommand(pluginDevCmd)

	pluginDevCmd.Flags().String("dir", ".", "plugin source directory containing plugin.json")
	pluginDevCmd.Flags().String("server", "http://127.0.0.1:5244", "alist server base URL (must be running with --dev)")
	pluginDevCmd.Flags().String("token", "", "admin token, copied from the admin panel [required] (falls back to $ALIST_ADMIN_TOKEN if unset)")
	pluginDevCmd.Flags().String("build", "go build -o {out} .", "shell build command; {out} is replaced with the staged binary's path")
	pluginDevCmd.Flags().Duration("interval", 700*time.Millisecond, "polling interval for source changes")
}

// resolveDevToken returns flagToken (trimmed) if it's non-empty, otherwise
// falls back to the ALIST_ADMIN_TOKEN environment variable (also trimmed).
// getenv is injected (production passes os.Getenv) so this is directly
// unit-testable without mutating process-global environment state.
func resolveDevToken(flagToken string, getenv func(string) string) string {
	if t := strings.TrimSpace(flagToken); t != "" {
		return t
	}
	return strings.TrimSpace(getenv("ALIST_ADMIN_TOKEN"))
}

// fileState is the mtime+size fingerprint snapshotTree records per file.
// Comparing size catches any content change even when the filesystem's mtime
// resolution is coarser than the edit-to-edit interval; comparing mtime
// catches same-size edits.
type fileState struct {
	size  int64
	mtime time.Time
}

// pluginDevSkipDirs names directories snapshotTree and stagePluginDir never
// descend into: .git (VCS metadata, frequently touched by the git process
// itself), dist/node_modules (build/dependency output - watching them would
// make every rebuild retrigger itself).
var pluginDevSkipDirs = map[string]bool{
	".git":         true,
	"dist":         true,
	"node_modules": true,
}

// snapshotTree walks dir and records path (relative to dir) -> mtime+size for
// every regular file outside pluginDevSkipDirs. It is the polling
// alternative to fsnotify: runPluginDev diffs two snapshots (via changed) to
// decide whether a rebuild is warranted.
func snapshotTree(dir string) (map[string]fileState, error) {
	snap := make(map[string]fileState)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && pluginDevSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		snap[rel] = fileState{size: info.Size(), mtime: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// changed reports whether two snapshotTree results differ: a different file
// set, or any shared path with a different size/mtime.
func changed(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return true
	}
	for path, av := range a {
		bv, ok := b[path]
		if !ok || av != bv {
			return true
		}
	}
	return false
}

// stagePluginDir assembles an install-package-shaped directory (plugin.json
// + i18n/ + a binary literally named "plugin", matching what
// extension.FindBinary/LoadManifest expect) in a fresh temp dir, out of a
// plugin source tree plus a just-built binary. It never copies source files
// (main.go, go.mod, ...) or VCS metadata - only the three things the loader
// looks at.
//
// The caller owns the returned directory, but must NOT remove it as soon as
// pushDevLoad returns: the server stores this exact path as the registry
// Entry's binPath and keeps using it for as long as the plugin stays
// registered - a lazily-Acquired process (e.g. no storage mounted yet at
// push time) execs it on first use, and a crashed subprocess's Restart execs
// it again, both potentially long after this request completed. Removing it
// right after the push would surface as ENOENT in either of those paths, not
// at push time. See runPluginDev's pluginDevStaging, which keeps the
// previously-pushed staging dir alive until the NEXT push succeeds (and only
// then reclaims it), and cleans up the last one when the watch loop exits.
func stagePluginDir(src, builtBin string) (string, error) {
	staging, err := os.MkdirTemp("", "alist-plugin-dev-*")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	// os.MkdirTemp defaults to 0700 (owner-only). The staged dir holds no
	// secrets - a manifest, i18n JSON, and the built binary, every one of
	// which is written world-readable below (0644/0755) - so widen the dir
	// itself to 0755: the CLI and the alist server it pushes to are commonly
	// run as different OS users (a dev box vs a service account), and a
	// server that cannot even stat/traverse into this dir cannot load the
	// staged plugin at all. This assumes the same-machine dev workflow
	// documented on --dev itself: a plugin package is already treated as
	// public once built (anyone who can install it can read its binary), so
	// nothing secret belongs in the plugin source tree being staged here.
	if err := os.Chmod(staging, 0o755); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("chmod staging dir: %w", err)
	}

	manifest, err := os.ReadFile(filepath.Join(src, "plugin.json"))
	if err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("read plugin.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "plugin.json"), manifest, 0o644); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("write staged plugin.json: %w", err)
	}

	i18nSrc := filepath.Join(src, "i18n")
	if info, statErr := os.Stat(i18nSrc); statErr == nil && info.IsDir() {
		if err := copyDirFlat(i18nSrc, filepath.Join(staging, "i18n")); err != nil {
			os.RemoveAll(staging)
			return "", fmt.Errorf("copy i18n: %w", err)
		}
	}

	binName := "plugin"
	if runtime.GOOS == "windows" {
		binName = "plugin.exe"
	}
	binData, err := os.ReadFile(builtBin)
	if err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("read built binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, binName), binData, 0o755); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("write staged binary: %w", err)
	}

	return staging, nil
}

// copyDirFlat recursively copies src's contents into dst (creating dst),
// preserving relative structure. Used for i18n/ only - a small, plain-file
// tree, so it makes no attempt to preserve symlinks or non-regular files.
func copyDirFlat(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// buildCommand resolves a build command string into the (name, args) argv
// alist plugin dev execs to run it, keyed on the host OS shell: `sh -c
// <script>` on every unix-like GOOS.
//
// windows has no /bin/sh, but passing the script inline as a `cmd /c`
// argument is a trap: Go's argv escaping and cmd.exe's own quote-stripping
// rules compose badly, so a script containing a double quote - the common
// case, e.g. `go build -ldflags="-X main.version=1.0" .` - comes out
// mangled on the other side. Sidestep the quoting entirely: on windows,
// write the resolved script verbatim to a temp .cmd file and point `cmd /c`
// at that file's path instead of at the script text. cmd.exe reads and runs
// the file's content unmodified, so any script content survives intact.
//
// "{out}" in script is replaced with out first, so the resolved command (or,
// on windows, the temp file's content) is what report shows verbatim on
// failure. cleanup removes the temp file on windows; it is a no-op on unix
// and must be called once the command has finished running (never before -
// cmd.exe still needs the file open). Split out of buildOnce so the argv
// shape - and, on windows, the temp file's content - is unit-testable
// without actually spawning a shell.
func buildCommand(goos, script, out string) (name string, args []string, cleanup func(), err error) {
	resolved := strings.ReplaceAll(script, "{out}", out)
	if goos != "windows" {
		return "sh", []string{"-c", resolved}, func() {}, nil
	}
	f, err := os.CreateTemp("", "alist-plugin-dev-build-*.cmd")
	if err != nil {
		return "", nil, func() {}, fmt.Errorf("create temp build script: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(resolved); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, func() {}, fmt.Errorf("write temp build script: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, func() {}, fmt.Errorf("close temp build script: %w", err)
	}
	return "cmd", []string{"/c", path}, func() { os.Remove(path) }, nil
}

// buildOnce runs buildCmd (with "{out}" replaced by outPath) via the host
// shell (buildCommand) in workDir, streaming its combined output to report.
// A non-zero exit becomes a plain error (never a panic), so a broken build
// never kills the watch loop above it.
func buildOnce(buildCmd, outPath, workDir string, report io.Writer) error {
	resolved := strings.ReplaceAll(buildCmd, "{out}", outPath)
	name, args, cleanup, err := buildCommand(runtime.GOOS, buildCmd, outPath)
	if err != nil {
		return fmt.Errorf("build command %q: %w", resolved, err)
	}
	defer cleanup()
	cmd := exec.Command(name, args...)
	cmd.Dir = workDir
	cmd.Stdout = report
	cmd.Stderr = report
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build command %q: %w", resolved, err)
	}
	return nil
}

// devLoadEnvelope mirrors the server's response shape (server/common.Resp):
// alist always answers HTTP 200 and puts the real result in the JSON body,
// so success/failure must be read from Code, never from the HTTP status.
type devLoadEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// devLoadResult mirrors DevLoadPlugin's success gin.H payload
// (server/handles/plugin_dev.go) - the envelope's .data on a 200 response.
type devLoadResult struct {
	ID               string   `json:"id"`
	Version          string   `json:"version"`
	Kinds            []string `json:"kinds"`
	ReloadedStorages int      `json:"reloaded_storages"`
}

// pushDevLoad POSTs {"dir": dir} to <server>/api/v4/admin/plugins/dev/load
// with the raw admin token (no "Bearer " prefix - matching the middleware's
// plain c.GetHeader("Authorization") check) and decodes the envelope. The
// request itself always succeeds over HTTP 200 even when the server rejected
// the load; that failure surfaces only via env.Code != 200, so it is checked
// explicitly rather than via resp.StatusCode. The one exception is HTTP 404:
// that means the route itself is not mounted (the target server was not
// started with --dev), which is a distinct, common misconfiguration worth a
// dedicated message instead of a generic "decode response" JSON error.
func pushDevLoad(server, token, dir string) (*devLoadResult, error) {
	payload, err := json.Marshal(map[string]string{"dir": dir})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	url := strings.TrimRight(server, "/") + "/api/v4/admin/plugins/dev/load"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("dev/load endpoint not found (http 404) - the target server must be started with --dev to expose it")
	}

	var env devLoadEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode response (http %d): %w", resp.StatusCode, err)
	}
	if env.Code != 200 {
		return nil, fmt.Errorf("dev/load rejected (code %d): %s", env.Code, env.Message)
	}

	var result devLoadResult
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &result); err != nil {
			return nil, fmt.Errorf("decode dev/load result data: %w", err)
		}
	}
	return &result, nil
}

// runPluginDevOnce is a single build -> stage -> push cycle: build opts.Dir
// with opts.BuildCmd into a scratch temp binary, assemble a staging dir in
// the install-package layout (stagePluginDir), and POST it to opts.Server
// (pushDevLoad). The scratch build dir is always cleaned up (its contents
// were only ever a source to copy the binary out of), but the STAGING dir is
// NOT removed on success - see stagePluginDir's doc comment: the server
// keeps using that exact path (Entry.binPath) long after this call returns,
// so ownership of the returned path transfers to the caller, which must keep
// it alive until a later successful push supersedes it (runPluginDev does
// this via pluginDevStaging). On any failure - including a rejected push -
// the staging dir (if one was created) is removed here since the server
// never durably adopted it, and "" is returned alongside the error. Any
// step's error is returned as-is (never panics) so a caller looping on this
// can log and keep watching.
func runPluginDevOnce(opts pluginDevOptions, report io.Writer) (string, error) {
	buildDir, err := os.MkdirTemp("", "alist-plugin-dev-build-*")
	if err != nil {
		return "", fmt.Errorf("create build temp dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	outPath := filepath.Join(buildDir, "plugin-built")
	if err := buildOnce(opts.BuildCmd, outPath, opts.Dir, report); err != nil {
		return "", err
	}

	staging, err := stagePluginDir(opts.Dir, outPath)
	if err != nil {
		return "", err
	}

	result, err := pushDevLoad(opts.Server, opts.Token, staging)
	if err != nil {
		os.RemoveAll(staging)
		return "", err
	}

	if result != nil && result.ID != "" {
		fmt.Fprintf(report, "plugin dev: %s@%s reloaded (storages: %d)\n", result.ID, result.Version, result.ReloadedStorages)
	} else {
		fmt.Fprintf(report, "plugin dev: reloaded from %s\n", opts.Dir)
	}
	return staging, nil
}

// pluginDevStaging tracks the single staging directory the running dev loop
// has most recently handed to the server. The server stores that path as its
// registry Entry's binPath and keeps exec'ing it for as long as the plugin
// stays registered (lazy Acquire on first real use, Restart after a crash) -
// see stagePluginDir's doc comment - so at most one staging dir may ever be
// reclaimed at a time, and only once a NEWER one has successfully replaced it
// server-side. This is what fixes the "push, then immediately delete"
// use-after-free: the previous round's directory survives until the next
// push actually succeeds.
type pluginDevStaging struct {
	active string
}

// rotate adopts next as the active staging dir (next must already have been
// successfully pushed to the server - i.e. it is runPluginDevOnce's returned
// path on a nil error) and removes whatever was active before, now that the
// server has moved on to next. A no-op if next is empty, which is what a
// FAILED runPluginDevOnce returns - a failed round must never touch the
// still-live previous staging dir.
func (s *pluginDevStaging) rotate(next string) {
	if next == "" || next == s.active {
		return
	}
	prev := s.active
	s.active = next
	if prev != "" {
		os.RemoveAll(prev)
	}
}

// cleanup removes the active staging dir, if any. Called once the watch loop
// exits (ctx canceled): nothing will ever dispatch to the server's binPath
// again, so the last staging dir this process handed it can finally go.
func (s *pluginDevStaging) cleanup() {
	if s.active != "" {
		os.RemoveAll(s.active)
		s.active = ""
	}
}

// runPluginDev runs the initial runPluginDevOnce, then polls opts.Dir every
// opts.Interval (snapshotTree/changed) and re-runs runPluginDevOnce -
// debounced by 300ms so a burst of saves (editor autosave, the build itself
// touching files) settles before triggering a rebuild - until ctx is
// canceled. A failed build/push is logged to report and never stops the
// loop. staging (pluginDevStaging) keeps exactly the last successfully-pushed
// staging dir alive across rounds - the server depends on that path staying
// put - and reclaims the previous round's dir only once a new one has taken
// its place; the final one is removed when this function returns.
func runPluginDev(ctx context.Context, opts pluginDevOptions, report io.Writer) {
	var staging pluginDevStaging
	defer staging.cleanup()

	if dir, err := runPluginDevOnce(opts, report); err != nil {
		fmt.Fprintf(report, "plugin dev: initial build/push failed: %v\n", err)
	} else {
		staging.rotate(dir)
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}

	last, _ := snapshotTree(opts.Dir)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := snapshotTree(opts.Dir)
			if err != nil {
				fmt.Fprintf(report, "plugin dev: watch error: %v\n", err)
				continue
			}
			if last != nil && !changed(last, snap) {
				continue
			}
			last = snap

			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			if settled, err := snapshotTree(opts.Dir); err == nil {
				last = settled
			}

			if dir, err := runPluginDevOnce(opts, report); err != nil {
				fmt.Fprintf(report, "plugin dev: rebuild/push failed: %v\n", err)
			} else {
				staging.rotate(dir)
			}
		}
	}
}
