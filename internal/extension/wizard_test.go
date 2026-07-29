package extension

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AlistGo/alist-ext"
)

func infoFormTestDoneSteps() []ext.WizardStep {
	return []ext.WizardStep{
		{ID: "welcome", Type: ext.WizardInfo, Title: map[string]string{"en": "Welcome"}},
		{ID: "config", Type: ext.WizardForm, Schema: &ext.JSONSchema{
			Type:     "object",
			Required: []string{"name"},
			Properties: map[string]*ext.JSONSchema{
				"name": {Type: "string"},
			},
		}},
		{ID: "probe", Type: ext.WizardTest, ProbeID: "connect"},
		{ID: "finish", Type: ext.WizardDone},
	}
}

func TestWizardHappyPathInfoFormTestDone(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	view, err := e.Start(context.Background(), "ext.a", infoFormTestDoneSteps(), "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Step == nil || view.Step.ID != "welcome" || view.Done {
		t.Fatalf("start: got %+v", view)
	}
	sid := view.SessionID

	// info -> form
	view, _ = e.Next(context.Background(), sid, nil, "")
	if view.Step == nil || view.Step.ID != "config" {
		t.Fatalf("after info: got %+v", view)
	}

	// bad form answer re-prompts SAME step with an error, does not advance
	view, _ = e.Next(context.Background(), sid, map[string]any{"name": ""}, "")
	if view.Step == nil || view.Step.ID != "config" || view.Error == "" {
		t.Fatalf("bad answer must re-prompt config with error: %+v", view)
	}

	// good form answer -> test
	view, _ = e.Next(context.Background(), sid, map[string]any{"name": "alice"}, "")
	if view.Step == nil || view.Step.ID != "probe" {
		t.Fatalf("after form: got %+v", view)
	}

	// test step with no registered prober auto-passes -> done
	view, _ = e.Next(context.Background(), sid, nil, "")
	if view.Step == nil || view.Step.ID != "finish" || !view.Done {
		t.Fatalf("after test: expected done finish step, got %+v", view)
	}
	if view.Note == "" {
		t.Fatalf("auto-passed test should carry a note")
	}
}

type failProber struct{}

func (failProber) Probe(ctx context.Context, id string, cfg map[string]any) error {
	return context.DeadlineExceeded
}

func TestWizardTestStepProbeFailureReprompts(t *testing.T) {
	RegisterWizardProber("ext.probe", failProber{})
	t.Cleanup(func() { RegisterWizardProber("ext.probe", nil) })

	e := NewWizardEngine(time.Minute)
	steps := []ext.WizardStep{
		{ID: "probe", Type: ext.WizardTest, ProbeID: "connect"},
		{ID: "done", Type: ext.WizardDone},
	}
	view, _ := e.Start(context.Background(), "ext.probe", steps, "")
	view, _ = e.Next(context.Background(), view.SessionID, nil, "")
	if view.Step == nil || view.Step.ID != "probe" || view.Error == "" {
		t.Fatalf("failed probe must re-prompt probe step with error: %+v", view)
	}
}

// TestWizardOAuthStepRequiresProvider replaces the old 501-stub test: an oauth
// step that declares no Provider can't mint an AuthURL, so it re-presents
// itself with a soft Error (not a hard Go error / NOT_IMPLEMENT) naming the
// problem — the wizard mechanism is now real (WS-8); this is the "misconfigured
// step" edge, not "not implemented".
func TestWizardOAuthStepRequiresProvider(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	steps := []ext.WizardStep{
		{ID: "oauth", Type: ext.WizardOAuth},
		{ID: "done", Type: ext.WizardDone},
	}
	view, err := e.Start(context.Background(), "ext.oauth", steps, "https://alist.example.com/api/v4/extensions/oauth/callback")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if view.Step == nil || view.Step.ID != "oauth" || view.Error == "" {
		t.Fatalf("oauth step with no provider must re-present with an error, got %+v", view)
	}
	if view.AuthURL != "" {
		t.Fatalf("no AuthURL should be minted when the step declares no provider, got %q", view.AuthURL)
	}

	// Next() on an unconfigured/incomplete oauth step must not advance either,
	// and must not return a hard Go error.
	view2, err := e.Next(context.Background(), view.SessionID, nil, "https://alist.example.com/api/v4/extensions/oauth/callback")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if view2.Done || view2.Step == nil || view2.Step.ID != "oauth" {
		t.Fatalf("incomplete oauth step must not advance: %+v", view2)
	}
}

func TestWizardChoiceBranchesToNamedStep(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	steps := []ext.WizardStep{
		{ID: "pick", Type: ext.WizardChoice, Options: []ext.WizardOption{
			{ID: "self-hosted", NextStep: "byo"},
			{ID: "official"},
		}},
		{ID: "skip", Type: ext.WizardInfo},
		{ID: "byo", Type: ext.WizardForm, Schema: &ext.JSONSchema{}},
		{ID: "done", Type: ext.WizardDone},
	}
	view, _ := e.Start(context.Background(), "ext.c", steps, "")
	view, _ = e.Next(context.Background(), view.SessionID, map[string]any{"option": "self-hosted"}, "")
	if view.Step == nil || view.Step.ID != "byo" {
		t.Fatalf("choice with next_step must jump to byo, got %+v", view)
	}
}

func TestWizardSessionExpiry(t *testing.T) {
	e := NewWizardEngine(10 * time.Millisecond)
	view, _ := e.Start(context.Background(), "ext.a", infoFormTestDoneSteps(), "")
	time.Sleep(30 * time.Millisecond)
	_, err := e.Next(context.Background(), view.SessionID, nil, "")
	if err != ErrWizardSessionExpired {
		t.Fatalf("expected ErrWizardSessionExpired, got %v", err)
	}
}

func TestWizardUnknownSession(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	if _, err := e.Next(context.Background(), "nope", nil, ""); err != ErrWizardSessionNotFound {
		t.Fatalf("expected ErrWizardSessionNotFound, got %v", err)
	}
}

func TestBuildWizardStepsTierWarnings(t *testing.T) {
	man := ext.Manifest{ID: "p", Permissions: ext.Permissions{Network: []string{"api.example.com"}}}
	declared := []ext.WizardStep{{ID: "cfg", Type: ext.WizardForm}}

	if steps := BuildWizardSteps(declared, TierOfficial, "", man); len(steps) != 1 {
		t.Fatalf("official should get no risk step, got %d steps", len(steps))
	}

	steps := BuildWizardSteps(declared, TierVerified, "myrepo", man)
	if len(steps) != 2 || steps[0].Type != ext.WizardRisk {
		t.Fatalf("verified should get a leading risk step, got %+v", steps)
	}
	if !strings.Contains(steps[0].Body["zh"], "myrepo") || !strings.Contains(steps[0].Body["en"], "myrepo") {
		t.Fatalf("verified risk body must name the vouching source: %q", steps[0].Body)
	}

	steps = BuildWizardSteps(declared, TierThirdParty, "", man)
	if len(steps) != 2 || steps[0].Type != ext.WizardRisk {
		t.Fatalf("third-party should get a leading risk step, got %+v", steps)
	}
	if !strings.Contains(steps[0].Body["zh"], "完整权限") || !strings.Contains(steps[0].Body["zh"], "存储凭证") {
		t.Fatalf("third-party risk body must state full-permission process + credential risk: %q", steps[0].Body["zh"])
	}
	if strings.Contains(steps[0].Body["zh"], "沙箱") || strings.Contains(steps[0].Body["en"], "sandbox") {
		t.Fatalf("risk body must not promise a sandbox anymore: %q", steps[0].Body)
	}

	declaredWithRisk := append([]ext.WizardStep{{ID: "fake", Type: ext.WizardRisk}}, declared...)
	steps = BuildWizardSteps(declaredWithRisk, TierOfficial, "", man)
	for _, s := range steps {
		if s.Type == ext.WizardRisk {
			t.Fatal("plugin-declared risk step must be stripped")
		}
	}
}
