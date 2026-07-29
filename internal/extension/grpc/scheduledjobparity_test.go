package grpc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchScheduledJobPlugin starts the scheduledjobplugin fixture over a real
// go-plugin subprocess. Mirrors launchEventHookPlugin one Kind over.
func launchScheduledJobPlugin(t *testing.T) *host.ScheduledJobClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchScheduledJob(context.Background(), exec.Command(scheduledJobPluginBin), nil)
	if err != nil {
		t.Fatalf("launch scheduledjob plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestScheduledJobGRPCRoundTrip is W7's gRPC round-trip gate: drive
// Schedule/Run against a real go-plugin subprocess. Schedule's spec crosses
// intact via ScheduleReply; Run's invocation is proven by the fixture's
// per-process run counter, echoed back via the Run error message (see the
// scheduledjobplugin fixture's doc comment — ext.ScheduledJob.Run has no
// positive result channel). Mirrors TestEventHookGRPCRoundTrip one Kind over.
func TestScheduledJobGRPCRoundTrip(t *testing.T) {
	c := launchScheduledJobPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testscheduledjob" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	if spec := c.Schedule(); spec != "@every 5s" {
		t.Fatalf("Schedule() = %q, want @every 5s", spec)
	}

	for i := 1; i <= 2; i++ {
		err := c.Run(ctx)
		if err == nil {
			t.Fatalf("Run #%d: want the fixture's counter error, got nil", i)
		}
		want := fmt.Sprintf("ran:%d", i)
		if err.Error() != want {
			t.Fatalf("Run #%d = %q, want %q (Run must actually be invoked in the subprocess)", i, err.Error(), want)
		}
	}
}

// TestScheduledJobGRPCDispatchThroughExtension drives the full host-side path
// this W7 delivers: extension.SetScheduledJobFactory -> Scheduler.Start -> a
// real gRPC subprocess's Schedule/Run, exactly the wiring
// server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireScheduledJob in place of this test's direct
// host.LaunchScheduledJob). The Scheduler swallows Run's return value into a
// log line (see runJobOnce), so this test drives the plugin with a fast
// ALIST_TEST_SCHEDULE and reads its ALIST_TEST_RUN_MARKER side file for
// out-of-band proof the tick actually reached the subprocess.
func TestScheduledJobGRPCDispatchThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	marker := filepath.Join(t.TempDir(), "runs.log")
	cmd := exec.Command(scheduledJobPluginBin)
	cmd.Env = append(os.Environ(),
		"ALIST_TEST_SCHEDULE=@every 20ms",
		"ALIST_TEST_RUN_MARKER="+marker,
	)
	c, err := host.LaunchScheduledJob(context.Background(), cmd, nil)
	if err != nil {
		t.Fatalf("launch scheduledjob plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	extension.SetScheduledJobFactory(func(e *extension.Entry) (ext.ScheduledJob, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetScheduledJobFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testscheduledjob",
		Kinds: []ext.Kind{ext.KindScheduledJob},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	sched := extension.NewScheduler(r)
	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	sched.Wait()

	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read run marker: %v (Scheduler never reached the gRPC subprocess's Run)", err)
	}
	if len(b) == 0 {
		t.Fatal("run marker is empty, want at least one Run recorded")
	}
}
