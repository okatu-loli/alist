package handles

// Red-team regressions for the federated-SSO auth surface (design §7). Two kinds
// of test live here:
//
//   - TestRedteam_*Refused / *Rejected / *NeverEchoed — attacks that the current
//     code correctly defeats. They are permanent regressions: if a future change
//     reopens the hole, these go red.
//
//   - TestRedteam_VULN_* — attacks that CURRENTLY SUCCEED. They pin the insecure
//     behavior so it is reproducible and tracked; each carries a comment stating
//     the root cause, impact, and the recommended fix (i.e. what the SECURE
//     behavior should be). These assert the (insecure) status quo so the suite
//     stays green while the finding is documented.
//
// Run with NO_PROXY=127.0.0.1,localhost (the mock IdP is loopback).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/authn/sso"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// ---------------------------------------------------------------------------
// Group A — attacks correctly REFUSED (permanent regressions)
// ---------------------------------------------------------------------------

// The most致命 face: an external IdP asserts a fresh subject whose VERIFIED email
// equals an admin's username (the email-as-username convention). The admin has NOT
// pre-linked this subject. Even though the SSO method is registered trusted, the
// identity resolver must refuse — an admin may sign in via SSO only through an
// explicit prior sub-link. Driven end-to-end through the real callback + mock IdP.
func TestRedteam_UnlinkedAdminEmailImpersonationRefused_EndToEnd(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })

	admin := mustAuthNUser(t, "admin-victim@corp.example", model.ADMIN, false)

	idp := newSSOMockIdP(t)
	idp.subject = "attacker-sub-forged"
	idp.email = "admin-victim@corp.example" // == the admin's username
	idp.emailVerified = true
	idp.nonce = "n"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	state := "state-admin-impersonate"
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "n", PKCEVerifier: "v", CreatedAt: time.Now()}, sso.StateTTL)

	w := doCallback(t, "sso:tprov", "c", state)
	if got := queryError(t, w); got != "denied" {
		t.Fatalf("an unlinked admin impersonated by verified email must be refused, got sso_error=%q", got)
	}
	if reloadUser(t, admin.ID).SsoID != "" {
		t.Fatal("a refused admin must NOT get the attacker's subject linked")
	}
}

// A spoofed subject that is not linked to the admin, presented at the resolver
// with the admin's verified email, is refused — the email path can never reach an
// admin, and the (unlinked) subject path finds nothing.
func TestRedteam_AdminSpoofedSubRefused(t *testing.T) {
	admin := mustAuthNUser(t, "root@corp.example", model.ADMIN, false)

	_, err := resolveFederatedUser("prov", sso.FederatedIdentity{
		Subject: "totally-made-up-sub", Email: "root@corp.example", EmailVerified: true,
	})
	if err == nil {
		t.Fatal("a made-up subject + admin's verified email must be refused")
	}
	if reloadUser(t, admin.ID).SsoID != "" {
		t.Fatal("the refused attempt must not link the spoofed subject to the admin")
	}
}

// An expired parked state is rejected on callback (stale/replay defense): Take
// evicts it and reports miss.
func TestRedteam_ExpiredStateRejected(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })

	state := "state-expired"
	// TTL already in the past → the entry is born expired.
	sso.DefaultStateStore.Put(state, sso.State{ProviderKey: "tprov", Nonce: "n", PKCEVerifier: "v", CreatedAt: time.Now()}, -time.Second)

	w := doCallback(t, "sso:tprov", "c", state)
	if got := queryError(t, w); got != "state" {
		t.Fatalf("an expired state must be rejected with sso_error=state, got %q", got)
	}
}

// A plain (login-mode) flow whose identity matches nothing — unknown subject,
// unverified email — must refuse and must never write a link, with auto-provision
// off. Confirms the login path cannot be coaxed into binding a subject to an
// arbitrary account.
func TestRedteam_UnmatchedLoginWritesNoLink(t *testing.T) {
	setAutoProvision(t, false)
	victim := mustAuthNUser(t, "bystander@corp.example", model.GENERAL, false)

	_, err := resolveFederatedUser("prov", sso.FederatedIdentity{
		Subject: "sub-x", Email: "bystander@corp.example", EmailVerified: false,
	})
	if err == nil {
		t.Fatal("an unverified-email login must be refused")
	}
	if reloadUser(t, victim.ID).SsoID != "" {
		t.Fatal("a refused login must not write any subject link")
	}
}

// The admin-facing provider view must never carry the client_secret, only a
// boolean that one is set. Guards against a regression that reintroduces the
// credential into an API response.
func TestRedteam_ClientSecretNeverEchoed(t *testing.T) {
	view := toSSOProviderView(sso.ProviderConfig{
		Key: "p", Name: "P", Issuer: "https://i", ClientID: "cid",
		ClientSecret: "s3cr3t-must-not-leak", Enabled: true,
	})
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "s3cr3t-must-not-leak") {
		t.Fatalf("client_secret value leaked into the admin view: %s", s)
	}
	if strings.Contains(s, "\"client_secret\"") {
		t.Fatalf("the admin view must not carry a client_secret field: %s", s)
	}
	if !strings.Contains(s, "\"client_secret_set\":true") {
		t.Fatalf("the admin view must report client_secret_set: %s", s)
	}
}

// ---------------------------------------------------------------------------
// Group B — attacks that CURRENTLY SUCCEED (documented findings; see report)
// ---------------------------------------------------------------------------

// FINDING 1 (HIGH) — a linked account is STILL takeover-able by any other subject
// that presents a verified email matching its username.
//
// Victim has already linked subject "sub-A" (their real identity). An attacker who
// can obtain email_verified=true for the victim's email (== username) from ANY
// configured provider — a lax/attacker-controlled IdP, a public multi-tenant IdP
// with self-asserted emails, or overlapping email namespaces across two configured
// providers — logs in as the victim with a DIFFERENT subject "sub-B".
//
// Root cause: resolveFederatedUser runs the verified-email match (step 2) whenever
// the INCOMING subject is not found, WITHOUT first checking whether the matched
// account already carries a sub-link. bindSSOToUser then no-ops (keeps sub-A) but
// the resolver still returns the victim → login. This defeats the design's "once
// linked, everything is by sub" invariant: linking provides zero protection.
//
// SECURE behavior would be: if the matched account already has a (different)
// sub-link, refuse the email path (an established link pins the identity; a new
// subject must go through explicit account-page linking, never silent email match).
//
// FIXED: an established sub-link pins the account — the email path is refused for
// a linked account, so a foreign subject cannot take it over.
func TestRedteam_VULN_LinkedAccountTakeoverByVerifiedEmail(t *testing.T) {
	victim := mustAuthNUser(t, "linked-victim@corp.example", model.GENERAL, false)
	setUserSSOID(t, victim, "prov:sub-A") // victim's real, established link

	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{
		Subject:       "sub-B", // a DIFFERENT subject the attacker controls
		Email:         "linked-victim@corp.example",
		EmailVerified: true,
	})
	if err == nil {
		t.Fatalf("REGRESSION: a foreign subject with a matching verified email took over a linked account (got user %v)", got)
	}
	// The established link is untouched.
	if reloadUser(t, victim.ID).SsoID != "prov:sub-A" {
		t.Fatalf("the victim's original link must be preserved, got %q", reloadUser(t, victim.ID).SsoID)
	}
}

// FINDING 2 (MEDIUM) — link-flow CSRF / identity fixation. The callback has no
// binding between the browser that completes it and the account that INITIATED the
// link flow (state.LinkUserID). An attacker starts a link flow for THEIR OWN
// account, then lures a victim (who has a live IdP session) into completing the
// authorization. The victim's real subject is then bound to the ATTACKER's
// account — so the victim's future SSO logins land in the attacker's account, and
// the attacker (who also holds that account's password) sees everything the victim
// does there.
//
// Precondition: the victim has not already linked this subject (else the conflict
// check trips). Realistic for a victim who has not yet used SSO.
//
// Root cause: no double-submit cookie / CSRF token tying the callback request to
// the initiator's browser session. Recommended fix: on SSOLinkBegin set a signed,
// HttpOnly cookie carrying the state id (or the LinkUserID); the callback must find
// it and match, so only the initiator's own browser can complete their link flow.
//
// FIXED: the callback requires the HttpOnly cookie SSOLinkBegin set (== state).
// The victim's browser, tricked into the attacker's authorize URL, carries no such
// cookie, so the link is refused and the victim's subject is NOT bound to the
// attacker's account.
func TestRedteam_VULN_LinkFlowCSRFBindsVictimSubToInitiator(t *testing.T) {
	sso.SetHTTPClient(noProxyHTTPClient())
	t.Cleanup(func() { sso.SetHTTPClient(nil) })

	attacker := mustAuthNUser(t, "attacker", model.GENERAL, false)

	idp := newSSOMockIdP(t)
	idp.subject = "victim-real-sub" // the victim authenticates at the IdP as themselves
	idp.email = "victim@corp.example"
	idp.emailVerified = true
	idp.nonce = "n"
	setSSOProvider(t, sso.ProviderConfig{Key: "tprov", Name: "T", Issuer: idp.issuer, ClientID: idp.clientID, Enabled: true})

	// The attacker's own link flow: LinkUserID = attacker.ID.
	state := "state-csrf-link"
	sso.DefaultStateStore.Put(state, sso.State{
		ProviderKey: "tprov", Nonce: "n", PKCEVerifier: "v", LinkUserID: attacker.ID, CreatedAt: time.Now(),
	}, sso.StateTTL)

	// The victim's browser completes the flow WITHOUT the link cookie (they never
	// started a link flow, so they hold no sso_link_state cookie).
	w := doCallback(t, "sso:tprov", "c", state)
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "sso_error") {
		t.Fatalf("REGRESSION: link callback without the binding cookie was not refused, landed at %q", loc)
	}
	if got := reloadUser(t, attacker.ID).SsoID; got != "" {
		t.Fatalf("REGRESSION: the victim's subject was bound to the attacker's account: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Group C — behavior pinned as ACCEPTABLE-BY-DESIGN (documented in report)
// ---------------------------------------------------------------------------

// A user who links their OWN subject as a non-admin and is LATER promoted to admin
// is reachable via SSO sub-login as that (now admin) account. There is no
// login-time IsAdmin re-gate, but this is acceptable: the link is the user's own
// deliberate act, and promotion is the operator's deliberate act. It is NOT the
// forbidden "unlinked admin via email" path. Pinned so the behavior is explicit.
func TestRedteam_PromotedLinkedUserReachableAsAdmin(t *testing.T) {
	u := mustAuthNUser(t, "promotable@corp.example", model.GENERAL, false)
	setUserSSOID(t, u, "prov:own-sub")

	// Operator promotes the account to admin.
	u = reloadUser(t, u.ID)
	u.Role = model.Roles{model.ADMIN}
	if err := op.UpdateUser(u); err != nil {
		t.Fatalf("promote: %v", err)
	}

	got, err := resolveFederatedUser("prov", sso.FederatedIdentity{Subject: "own-sub", Email: "promotable@corp.example", EmailVerified: true})
	if err != nil {
		t.Fatalf("a self-linked, later-promoted user should resolve by sub: %v", err)
	}
	if got.ID != u.ID || !got.IsAdmin() {
		t.Fatal("expected the now-admin account to resolve via its own prior sub-link")
	}
}
