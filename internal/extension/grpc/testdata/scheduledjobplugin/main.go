// Command scheduledjobplugin serves a synthetic Kind=ScheduledJob extension as
// a go-plugin gRPC plugin (the time-driven background-task mechanism-proving
// test double — v4 ships no real ScheduledJob plugin yet). ext.ScheduledJob.Run
// returns only an error (no positive result channel), so this fixture proves
// each Run reached the subprocess two ways:
//   - a direct Run() caller sees an error encoding a per-process,
//     monotonically increasing counter ("ran:1", "ran:2", ...);
//   - when ALIST_TEST_RUN_MARKER is set (the Scheduler-driven dispatch test,
//     which never reads Run's swallowed-and-logged return value), each Run
//     also appends a line to that file, giving the host test an out-of-band
//     signal the ticker actually fired in the subprocess.
//
// ALIST_TEST_SCHEDULE overrides the cadence Schedule() reports (default
// "@every 5s") so a Scheduler-driven test can use a fast tick instead of
// waiting out the default. Mirrors eventhookplugin (testdata/eventhookplugin)
// one Kind over.
package main

import (
	"context"
	"fmt"
	"os"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type scheduledJobPlugin struct {
	runs int
}

var _ ext.ScheduledJob = (*scheduledJobPlugin)(nil)

func (p *scheduledJobPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testscheduledjob",
		Name:    map[string]string{"en": "Test Scheduled Job"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindScheduledJob},
	}
}

func (p *scheduledJobPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *scheduledJobPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *scheduledJobPlugin) Schedule() string {
	if spec := os.Getenv("ALIST_TEST_SCHEDULE"); spec != "" {
		return spec
	}
	return "@every 5s"
}

func (p *scheduledJobPlugin) Run(ctx context.Context) error {
	p.runs++
	if marker := os.Getenv("ALIST_TEST_RUN_MARKER"); marker != "" {
		f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(fmt.Sprintf("ran:%d\n", p.runs))
			_ = f.Close()
		}
	}
	return fmt.Errorf("ran:%d", p.runs)
}

func main() {
	grpcplugin.ServeScheduledJob(&scheduledJobPlugin{})
}
