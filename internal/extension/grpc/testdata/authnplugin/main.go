// Command authnplugin serves a synthetic Kind=AuthNMethod extension as a
// go-plugin gRPC plugin (design §2.2's mechanism-proving test double — v4 ships
// no real SSO/LDAP plugin yet). It exercises the AuthNMethod round trip so the
// host bridge and login orchestrator can be tested against a real subprocess
// without a real IdP.
//
// The method it serves is selected by the AUTHNPLUGIN_MODE environment variable
// (inherited from the parent, since host.LaunchAuthNMethod leaves exec.Cmd.Env
// nil):
//
//   - "primary" (default): a synthetic LDAP-style primary method. Finish
//     verifies a fixed password ("x") and asserts the identity the caller
//     supplied — deliberately willing to assert ANY username, so tests can prove
//     the HOST (not the plugin) is what caps privilege and gates provisioning.
//   - "federated": a synthetic OIDC-style federated method. Begin returns a
//     redirect URL; Finish asserts an identity plus an auto-provision hint. The
//     host still refuses to create the user unless its auto-provision policy is
//     on, and never as an admin.
//
// This plugin never issues a token — it only reports "verified: true/false" and
// a username. That is the whole security point of the AuthNMethod contract.
package main

import (
	"context"
	"os"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

const envMode = "AUTHNPLUGIN_MODE"

// fixedPassword is the credential the primary method accepts. A wrong password
// yields Verified=false (a clean rejection), never an error.
const fixedPassword = "x"

type authnPlugin struct {
	federated bool
}

var _ ext.AuthNMethod = (*authnPlugin)(nil)

func (p *authnPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testauthn",
		Name:    map[string]string{"en": "Test AuthN Method"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindAuthNMethod},
	}
}

func (p *authnPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *authnPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *authnPlugin) MethodInfo() ext.AuthMethodInfo {
	if p.federated {
		return ext.AuthMethodInfo{ID: "federated", Kind: ext.AuthFederated, Display: map[string]string{"en": "Test IdP"}}
	}
	return ext.AuthMethodInfo{ID: "primary", Kind: ext.AuthPrimary, Display: map[string]string{"en": "Test LDAP"}}
}

func (p *authnPlugin) Begin(ctx context.Context, principal ext.Principal, req ext.AuthNRequest) (ext.AuthNChallenge, error) {
	if p.federated {
		// A real OIDC method would send the browser to the IdP authorization URL,
		// round-tripping req.State. This synthetic one just echoes a URL.
		return ext.AuthNChallenge{
			ChallengeID: "chal-" + req.State,
			RedirectURL: req.RedirectURI + "?state=" + req.State,
		}, nil
	}
	// A primary/LDAP method has no redirect; the client posts credentials
	// directly to Finish. Begin only mints a correlation id.
	return ext.AuthNChallenge{ChallengeID: "chal-primary"}, nil
}

func (p *authnPlugin) Finish(ctx context.Context, req ext.AuthNResponse) (ext.AuthNResult, error) {
	username := req.Params["username"]
	if p.federated {
		if username == "" {
			username = "fed-user"
		}
		// The IdP asserts an identity; the host decides whether to provision it.
		return ext.AuthNResult{
			Username: username,
			Verified: true,
			AutoProvision: &ext.ProvisionHint{
				DisplayName: username,
				Email:       username + "@idp.example.com",
			},
		}, nil
	}
	if req.Params["password"] != fixedPassword {
		return ext.AuthNResult{Verified: false}, nil
	}
	if username == "" {
		username = "alice"
	}
	return ext.AuthNResult{Username: username, Verified: true}, nil
}

func main() {
	p := &authnPlugin{federated: os.Getenv(envMode) == "federated"}
	grpcplugin.ServeAuthNMethod(p)
}
