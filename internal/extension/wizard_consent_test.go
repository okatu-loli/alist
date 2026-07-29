package extension

import (
	"context"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// TestWizardPermissionConsentCapturesApproval verifies the permission-consent
// step both gates on consent and carries the admin's approved selection out in
// the wizard's merged answers, so a wizard-driven install can forward it to
// SetGrant (design §6.4).
func TestWizardPermissionConsentCapturesApproval(t *testing.T) {
	e := NewWizardEngine(time.Minute)
	steps := []ext.WizardStep{
		{ID: "consent", Type: ext.WizardPermissionConsent},
		{ID: "done", Type: ext.WizardDone},
	}
	view, err := e.Start(context.Background(), "ext.consent", steps, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Missing consent re-prompts the same step.
	view, _ = e.Next(context.Background(), view.SessionID, map[string]any{"consent": false}, "")
	if view.Done || view.Step == nil || view.Step.ID != "consent" || view.Error == "" {
		t.Fatalf("consent=false should re-prompt with an error, got %+v", view)
	}

	// Approving with a granted selection advances and carries the selection out.
	granted := map[string]any{
		"host_apis": []any{map[string]any{"name": "events"}},
		"network":   []any{"api.example.com"},
	}
	view, _ = e.Next(context.Background(), view.SessionID,
		map[string]any{"consent": true, "granted": granted}, "")
	if !view.Done {
		t.Fatalf("consent=true should advance to done, got %+v", view)
	}
	g, ok := view.Answers["granted"].(map[string]any)
	if !ok {
		t.Fatalf("merged answers should carry the granted selection, got %+v", view.Answers)
	}
	if _, ok := g["host_apis"]; !ok {
		t.Fatalf("granted selection lost its host_apis: %+v", g)
	}
}
