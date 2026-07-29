package handles

// Red-team probes against the built-in passkey login surface (the trusted
// AuthNMethod in internal/authn/passkey, wired through authn.go's begin/finish).
// These construct concrete attacks with a real virtual authenticator and assert
// the HOST refuses them; they complement passkey_login_test.go (happy paths +
// the forged-userHandle-with-own-credential case) and authn_test.go's trust
// carve-out. Every test here is a permanent regression: it must stay green.
//
// The two sharpest vectors the design flags are covered end to end:
//   - trust elevation to an admin session (self-reported / name-squatted trust)
//   - reverse-lookup forgery riding a real credential id into another account
//
// plus the crypto/anti-replay backstops (signature layer, challenge binding).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authn/passkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/descope/virtualwebauthn"
	"github.com/gin-gonic/gin"
)

// registerBuiltinPasskey installs the real trusted built-in passkey method and
// tears it down after the test, so each probe drives the genuine login path.
func registerBuiltinPasskey(t *testing.T) {
	t.Helper()
	passkey.RegisterBuiltin()
	t.Cleanup(func() { extension.RegisterTrustedAuthNMethodInstance(passkey.BuiltinMethodID, nil) })
}

// beginBuiltinLogin drives the anonymous begin endpoint and returns the challenge
// id plus the parsed assertion options the authenticator must sign.
func beginBuiltinLogin(t *testing.T) (string, *virtualwebauthn.AssertionOptions) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bw := httptest.NewRecorder()
	bc, _ := gin.CreateTestContext(bw)
	breq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/begin", bytes.NewReader([]byte("{}")))
	breq.Header.Set("Content-Type", "application/json")
	bc.Request = breq
	bc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	AuthNMethodBegin(bc)

	var begin struct {
		Code int `json:"code"`
		Data struct {
			ChallengeID string `json:"challenge_id"`
			Data        []byte `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bw.Body.Bytes(), &begin); err != nil {
		t.Fatalf("decode begin: %v (body=%s)", err, bw.Body.String())
	}
	if begin.Code != 200 || begin.Data.ChallengeID == "" {
		t.Fatalf("begin should return a challenge: %s", bw.Body.String())
	}
	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(begin.Data.Data))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v (data=%s)", err, string(begin.Data.Data))
	}
	return begin.Data.ChallengeID, asOpts
}

// finishBuiltinLogin posts a signed assertion (data) for challengeID to the
// anonymous finish endpoint, resetting the per-IP throttle so a probe is judged
// on its own merits.
func finishBuiltinLogin(t *testing.T, challengeID string, data []byte) (*httptest.ResponseRecorder, authnFinishResp) {
	t.Helper()
	fw := httptest.NewRecorder()
	fc, _ := gin.CreateTestContext(fw)
	body, _ := json.Marshal(map[string]any{"challenge_id": challengeID, "data": data})
	freq := httptest.NewRequest("POST", "/api/v4/auth/method/"+passkey.BuiltinMethodID+"/finish", bytes.NewReader(body))
	freq.Header.Set("Content-Type", "application/json")
	fc.Request = freq
	fc.Params = gin.Params{{Key: "id", Value: passkey.BuiltinMethodID}}
	authnThrottle.Reset(fc.ClientIP())
	AuthNMethodFinish(fc)
	var resp authnFinishResp
	_ = json.Unmarshal(fw.Body.Bytes(), &resp)
	return fw, resp
}

// --- Vector 1 (CRITICAL): reverse-lookup forgery that rides a REAL credential id
// into an admin account. Unlike the own-credential forgery already covered, here
// the attacker copies the admin's public credential id (userHandle + rawID both
// resolve to the admin, so the ownership check PASSES) but signs with their OWN
// private key. The last line of defense is the cryptographic signature check
// against the admin's stored public key, which must reject the forged signature.
func TestRedteamPasskeyAdminCredIDWithAttackerKeyRejected(t *testing.T) {
	pinWebauthnRP(t)
	registerBuiltinPasskey(t)

	admin := mustAuthNUser(t, "rt-pk-admin-key", model.ADMIN, false)
	adminF := newPkFixture(t, admin) // enrolls the admin's genuine passkey

	// Confirm we truly reuse the admin's stored credential id, so the reverse
	// lookup + ownership check PASS and the signature check is the guard on trial.
	adminPks, _ := model.ParsePasskeys(mustReload(t, admin.ID).Authn)
	if len(adminPks) != 1 {
		t.Fatalf("admin should have exactly one enrolled passkey, got %d", len(adminPks))
	}
	if got := base64.RawURLEncoding.EncodeToString(adminF.cred.ID); got != adminPks[0].ID {
		t.Fatalf("test setup: forged cred id %q must equal the admin's stored id %q (else ownership, not signature, is what rejects)", got, adminPks[0].ID)
	}

	// Forge a credential: the admin's raw id, but a freshly minted attacker key.
	attackerKey := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	forged := virtualwebauthn.Credential{ID: adminF.cred.ID, Key: attackerKey.Key, Counter: 1}
	forgedAuth := virtualwebauthn.NewAuthenticator()
	forgedAuth.Options.UserHandle = admin.WebAuthnID() // point the reverse-lookup at the admin
	forgedAuth.AddCredential(forged)

	rp := virtualwebauthn.RelyingParty{ID: pkTestRPID, Name: "Example", Origin: pkTestOrigin}
	cid, asOpts := beginBuiltinLogin(t)
	asResp := virtualwebauthn.CreateAssertionResponse(rp, forgedAuth, forged, *asOpts)

	w, resp := finishBuiltinLogin(t, cid, []byte(asResp))
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("an assertion reusing the admin's credential id but signed with a foreign key must mint NO session — INVARIANT BREACH: http=%d body=%s", w.Code, w.Body.String())
	}
	// The admin's stored counter must be untouched: a rejected login never writes
	// back (no clone-warning poisoning of the real key either).
	after, _ := model.ParsePasskeys(mustReload(t, admin.ID).Authn)
	if len(after) != 1 || after[0].SignCount != adminPks[0].SignCount {
		t.Fatalf("a rejected forgery must not mutate the admin's stored passkey: before=%d after=%+v", adminPks[0].SignCount, after)
	}
}

// --- Vector 2 (CRITICAL): trust cannot be name-squatted. An attacker who manages
// to register a method under the built-in's very id "builtin.passkey" through the
// PLAIN (untrusted) path must NOT inherit admin-login trust. Trust is a host-side
// registration fact keyed to the trusted registration call, not to the id string,
// so a same-named plain method is still barred from minting an admin session.
func TestRedteamPlainBuiltinIdCannotLoginAdmin(t *testing.T) {
	mustAuthNUser(t, "rt-squat-admin", model.ADMIN, false)
	// Register a hostile method under the EXACT built-in id, but plainly (untrusted).
	extension.RegisterAuthNMethodInstance(passkey.BuiltinMethodID, &stubAuthN{
		id: passkey.BuiltinMethodID, kind: ext.AuthPrimary, verified: true, username: "rt-squat-admin",
	})
	t.Cleanup(func() { extension.RegisterAuthNMethodInstance(passkey.BuiltinMethodID, nil) })

	if extension.DefaultRegistry.AuthNMethodTrusted(passkey.BuiltinMethodID) {
		t.Fatal("a plainly-registered method must not be trusted even under the built-in id")
	}
	_, resp := doAuthNFinish(t, passkey.BuiltinMethodID, nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a name-squatted plain method must never mint an admin session: %+v", resp)
	}
}

// --- Vector 2b: a method cannot self-report trust. The host reads trust only from
// its own registry (AuthNMethodTrusted), never from anything the method returns.
// A plainly-registered stub asserting an admin — no matter what its MethodInfo
// says — is refused, and the registry reports it untrusted.
func TestRedteamMethodCannotSelfReportTrust(t *testing.T) {
	mustAuthNUser(t, "rt-selftrust-admin", model.ADMIN, false)
	registerStubAuthN(t, &stubAuthN{id: "rt.selftrust", kind: ext.AuthPrimary, verified: true, username: "rt-selftrust-admin"})

	if extension.DefaultRegistry.AuthNMethodTrusted("rt.selftrust") {
		t.Fatal("a plain in-process method must be reported untrusted")
	}
	_, resp := doAuthNFinish(t, "rt.selftrust", nil)
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("an untrusted method asserting an admin must be refused: %+v", resp)
	}
}

// --- Vector 3: a captured assertion cannot be replayed, even when the
// authenticator's signature counter never advances (the counter==0 exception the
// spec permits). Anti-replay does not lean on the counter here — it leans on the
// single-use, per-ceremony challenge bound into clientDataJSON. So a valid
// assertion redeemed once is refused (a) again on its own challenge (single use)
// and (b) against a fresh challenge (the signed challenge no longer matches).
func TestRedteamCounterZeroAssertionCannotBeReplayed(t *testing.T) {
	pinWebauthnRP(t)
	registerBuiltinPasskey(t)

	user := mustAuthNUser(t, "rt-pk-replay", model.GENERAL, false)
	f := newPkFixture(t, user)
	rp := virtualwebauthn.RelyingParty{ID: pkTestRPID, Name: "Example", Origin: pkTestOrigin}

	// Sign an assertion at counter 0 (authenticator reports no counter support).
	cid1, asOpts1 := beginBuiltinLogin(t)
	f.cred.Counter = 0
	captured := []byte(virtualwebauthn.CreateAssertionResponse(rp, f.auth, f.cred, *asOpts1))

	// First redemption succeeds (counter 0 is a permitted no-clone exception).
	if _, resp := finishBuiltinLogin(t, cid1, captured); resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("a genuine counter-0 assertion should log in once: %+v", resp)
	}
	// Replay on the SAME challenge: the challenge was consumed (single use).
	if _, resp := finishBuiltinLogin(t, cid1, captured); resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("replaying a consumed challenge must mint nothing: %+v", resp)
	}
	// Replay the SAME bytes against a FRESH challenge: the assertion signed the old
	// challenge, so it cannot satisfy the new one — no counter movement required.
	cid2, _ := beginBuiltinLogin(t)
	if _, resp := finishBuiltinLogin(t, cid2, captured); resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("an assertion bound to a spent challenge must not satisfy a fresh one: %+v", resp)
	}
}

// --- Vector 4: the ownership guard is not admin-specific. A forged userHandle
// pointing at ANY other existing account (here a second non-admin) while signing
// with the attacker's own credential is refused just the same — the resolved
// victim does not own the asserted credential id.
func TestRedteamForgedUserHandleToNonAdminRejected(t *testing.T) {
	pinWebauthnRP(t)
	registerBuiltinPasskey(t)

	owner := mustAuthNUser(t, "rt-pk-owner2", model.GENERAL, false)
	victim := mustAuthNUser(t, "rt-pk-victim2", model.GENERAL, false)
	f := newPkFixture(t, owner)

	_, resp := f.login(t, 1, victim.WebAuthnID())
	if resp.Code == 200 || resp.Data.Token != "" {
		t.Fatalf("a forged handle onto another non-admin must mint no session: %+v", resp)
	}
	// The victim gains no credential and no session bleed.
	if pks, _ := model.ParsePasskeys(mustReload(t, victim.ID).Authn); len(pks) != 0 {
		t.Fatalf("the victim account must remain credential-less: %+v", pks)
	}
}

// --- Vector 5: the self-management list must never leak crypto material. The
// account page needs only labels/timestamps; the COSE public key and the sign
// counter must never cross the wire (they are the anti-clone/verification secrets).
func TestRedteamPasskeyListNeverLeaksSecrets(t *testing.T) {
	pinWebauthnRP(t)
	user := mustAuthNUser(t, "rt-pk-leak", model.GENERAL, false)
	auth := virtualwebauthn.NewAuthenticator()
	auth.Options.UserHandle = user.WebAuthnID()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	httpRegisterPasskey(t, user, auth, cred, "leaky?")

	reloaded := mustReload(t, user.ID)
	lw, lc := authedCtx(t, reloaded, nil, nil)
	PasskeyList(lc)
	raw := lw.Body.String()
	for _, forbidden := range []string{"public_key", "publicKey", "sign_count", "signCount", "SignCount"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("passkey list leaked secret field %q: %s", forbidden, raw)
		}
	}
}

// --- Vector 6: enrollment is caller-bound, not userHandle-bound. Even if the
// authenticator advertises a foreign userHandle during registration, the new
// credential is stored on the AUTHENTICATED caller's account only — a passkey can
// never be planted on someone else's account through a forged handle.
func TestRedteamRegistrationIgnoresForgedUserHandle(t *testing.T) {
	pinWebauthnRP(t)
	attacker := mustAuthNUser(t, "rt-pk-enroller", model.GENERAL, false)
	victim := mustAuthNUser(t, "rt-pk-enroll-victim", model.GENERAL, false)

	// The attacker's authenticator claims the VICTIM's user handle.
	auth := virtualwebauthn.NewAuthenticator()
	auth.Options.UserHandle = victim.WebAuthnID()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	credID := httpRegisterPasskey(t, attacker, auth, cred, "planted?")

	// The credential landed on the attacker (the caller), not the victim.
	if pks, _ := model.ParsePasskeys(mustReload(t, attacker.ID).Authn); len(pks) != 1 || pks[0].ID != credID {
		t.Fatalf("enrolled credential must belong to the caller: %+v", pks)
	}
	if pks, _ := model.ParsePasskeys(mustReload(t, victim.ID).Authn); len(pks) != 0 {
		t.Fatalf("a forged user handle must not plant a credential on the victim: %+v", pks)
	}
}

// mustReload fetches the freshest copy of a user (Authn column) or fails.
func mustReload(t *testing.T, id uint) *model.User {
	t.Helper()
	u, err := op.GetUserById(id)
	if err != nil {
		t.Fatalf("reload user %d: %v", id, err)
	}
	return u
}
