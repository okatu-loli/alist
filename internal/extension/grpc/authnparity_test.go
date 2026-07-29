package grpc

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchAuthNPlugin starts the authnplugin fixture in the given mode
// ("primary" or "federated"). host.LaunchAuthNMethod uses exec.Cmd with a nil
// Env, so the child inherits the parent's environment — t.Setenv here is
// visible to the subprocess it launches.
func launchAuthNPlugin(t *testing.T, mode string) *host.AuthNClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	t.Setenv("AUTHNPLUGIN_MODE", mode)
	c, err := host.LaunchAuthNMethod(context.Background(), exec.Command(authnPluginBin), nil)
	if err != nil {
		t.Fatalf("launch authn plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestAuthNMethodGRPCPrimaryRoundTrip is the AuthNMethod gRPC round-trip gate:
// drive MethodInfo/Begin/Finish against a real go-plugin subprocess and confirm
// the identity assertion crosses the wire intact. Note the plugin returns only
// "verified + username" — never a token.
func TestAuthNMethodGRPCPrimaryRoundTrip(t *testing.T) {
	c := launchAuthNPlugin(t, "primary")
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testauthn" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}
	if info := c.MethodInfo(); info.Kind != ext.AuthPrimary || info.ID != "primary" {
		t.Fatalf("MethodInfo = %+v", info)
	}

	chal, err := c.Begin(ctx, "", ext.AuthNRequest{State: "s1"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if chal.ChallengeID == "" {
		t.Fatalf("Begin returned empty challenge id")
	}

	// Correct credential verifies and asserts the identity.
	res, err := c.Finish(ctx, ext.AuthNResponse{Params: map[string]string{"username": "alice", "password": "x"}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !res.Verified || res.Username != "alice" {
		t.Fatalf("Finish result = %+v, want verified alice", res)
	}

	// Wrong credential must round-trip as a clean unverified result, not an error.
	bad, err := c.Finish(ctx, ext.AuthNResponse{Params: map[string]string{"username": "alice", "password": "nope"}})
	if err != nil {
		t.Fatalf("Finish(bad): unexpected error %v", err)
	}
	if bad.Verified {
		t.Fatal("wrong password must not verify across the wire")
	}
}

// TestAuthNMethodGRPCFederatedRoundTrip confirms the federated shape crosses the
// wire: Begin yields a redirect URL that round-trips state, and Finish carries
// the auto-provision hint the host consults only under its policy gate.
func TestAuthNMethodGRPCFederatedRoundTrip(t *testing.T) {
	c := launchAuthNPlugin(t, "federated")
	ctx := context.Background()

	if info := c.MethodInfo(); info.Kind != ext.AuthFederated {
		t.Fatalf("MethodInfo kind = %v, want federated", info.Kind)
	}

	chal, err := c.Begin(ctx, "", ext.AuthNRequest{RedirectURI: "https://alist.example.com/cb", State: "st-9"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !strings.Contains(chal.RedirectURL, "st-9") {
		t.Fatalf("Begin must round-trip state into redirect URL, got %q", chal.RedirectURL)
	}

	res, err := c.Finish(ctx, ext.AuthNResponse{Params: map[string]string{"username": "carol"}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !res.Verified || res.Username != "carol" {
		t.Fatalf("Finish result = %+v", res)
	}
	if res.AutoProvision == nil || res.AutoProvision.Email != "carol@idp.example.com" {
		t.Fatalf("federated Finish must carry the provision hint across the wire, got %+v", res.AutoProvision)
	}
}
