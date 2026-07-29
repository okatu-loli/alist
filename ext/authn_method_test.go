package ext

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeAuthNMethod is a minimal in-process ext.AuthNMethod that pins the
// interface's compile-time shape. It models a synthetic primary method that
// accepts a fixed credential and reports the verified identity — the plugin
// only PROVES identity; the host decides whether to mint a session.
type fakeAuthNMethod struct{}

func (fakeAuthNMethod) Manifest() Manifest                     { return Manifest{ID: "com.example.authn"} }
func (fakeAuthNMethod) Init(ctx context.Context, h Host) error { return nil }
func (fakeAuthNMethod) Shutdown(ctx context.Context) error     { return nil }

func (fakeAuthNMethod) MethodInfo() AuthMethodInfo {
	return AuthMethodInfo{ID: "primary", Kind: AuthPrimary, Display: map[string]string{"en": "Test"}}
}

func (fakeAuthNMethod) Begin(ctx context.Context, principal Principal, req AuthNRequest) (AuthNChallenge, error) {
	return AuthNChallenge{ChallengeID: "chal-1"}, nil
}

func (fakeAuthNMethod) Finish(ctx context.Context, req AuthNResponse) (AuthNResult, error) {
	if req.Params["password"] != "x" {
		return AuthNResult{Verified: false}, nil
	}
	return AuthNResult{Username: req.Params["username"], Verified: true}, nil
}

var _ AuthNMethod = fakeAuthNMethod{}

func TestAuthNMethodInterfaceShape(t *testing.T) {
	var m AuthNMethod = fakeAuthNMethod{}
	if m.MethodInfo().Kind != AuthPrimary {
		t.Fatalf("MethodInfo kind = %v", m.MethodInfo().Kind)
	}
	chal, err := m.Begin(context.Background(), "", AuthNRequest{RedirectURI: "https://host/cb", State: "s1"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if chal.ChallengeID == "" {
		t.Fatal("Begin returned empty challenge id")
	}
	res, err := m.Finish(context.Background(), AuthNResponse{Params: map[string]string{"username": "alice", "password": "x"}})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !res.Verified || res.Username != "alice" {
		t.Fatalf("Finish result = %+v", res)
	}
	// A wrong credential must resolve to unverified, never an error-shaped pass.
	bad, err := m.Finish(context.Background(), AuthNResponse{Params: map[string]string{"username": "alice", "password": "nope"}})
	if err != nil {
		t.Fatalf("Finish(bad): %v", err)
	}
	if bad.Verified {
		t.Fatal("wrong credential must not verify")
	}
}

// TestAuthNResultRoundTrip pins the wire/JSON shape of the identity assertion,
// including the optional ProvisionHint the host consults only under the
// federated auto-provision policy gate.
func TestAuthNResultRoundTrip(t *testing.T) {
	res := AuthNResult{
		Username: "bob",
		Verified: true,
		AutoProvision: &ProvisionHint{
			DisplayName: "Bob Example",
			Email:       "bob@example.com",
			Attributes:  map[string]string{"dept": "eng"},
		},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AuthNResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Username != "bob" || !got.Verified || got.AutoProvision == nil {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.AutoProvision.Email != "bob@example.com" || got.AutoProvision.Attributes["dept"] != "eng" {
		t.Fatalf("provision hint round trip mismatch: %+v", got.AutoProvision)
	}
}

// TestAuthKindValues pins the AuthKind enum ordering, which the wire codec and
// the host classifier both depend on.
func TestAuthKindValues(t *testing.T) {
	if AuthPrimary != 0 || AuthSecondFactor != 1 || AuthFederated != 2 {
		t.Fatalf("AuthKind values drifted: primary=%d second=%d federated=%d",
			AuthPrimary, AuthSecondFactor, AuthFederated)
	}
}
