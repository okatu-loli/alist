package passkey

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/descope/virtualwebauthn"
	"github.com/go-webauthn/webauthn/webauthn"
)

// ceremonyFixture wires a real go-webauthn Relying Party against a descope
// virtual authenticator so registration and login ceremonies run end to end with
// genuine attestation/assertion signatures — no HTTP, no browser.
type ceremonyFixture struct {
	w     *webauthn.WebAuthn
	rp    virtualwebauthn.RelyingParty
	auth  virtualwebauthn.Authenticator
	cred  virtualwebauthn.Credential
	user  *model.User
	store *ChallengeStore
}

const (
	testRPID    = "example.com"
	testOrigin  = "https://example.com"
	testWrongRP = "https://evil.com"
)

func newCeremonyFixture(t *testing.T) *ceremonyFixture {
	t.Helper()
	w, err := New(RP{ID: testRPID, DisplayName: "Example", Origins: []string{testOrigin}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	user := &model.User{ID: 42, Username: "alice"}
	auth := virtualwebauthn.NewAuthenticator()
	// The discoverable-login assertion carries userHandle back to the RP; it must
	// equal the user's WebAuthn handle so the handler can resolve the account.
	auth.Options.UserHandle = user.WebAuthnID()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	return &ceremonyFixture{
		w:     w,
		rp:    virtualwebauthn.RelyingParty{ID: testRPID, Name: "Example", Origin: testOrigin},
		auth:  auth,
		cred:  cred,
		user:  user,
		store: NewChallengeStore(),
	}
}

// register runs a full registration ceremony via the challenge store and appends
// the resulting passkey to the user's Authn column.
func (f *ceremonyFixture) register(t *testing.T, name string) *model.Passkey {
	t.Helper()
	creation, session, err := BeginRegistration(f.w, f.user)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	const cid = "reg-challenge"
	f.store.Put(cid, session, time.Minute)

	optsJSON, err := json.Marshal(creation)
	if err != nil {
		t.Fatalf("marshal creation: %v", err)
	}
	attOpts, err := virtualwebauthn.ParseAttestationOptions(string(optsJSON))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(f.rp, f.auth, f.cred, *attOpts)

	sess, ok := f.store.Take(cid)
	if !ok {
		t.Fatal("challenge should be present for finish")
	}
	pk, err := FinishRegistration(f.w, f.user, *sess, []byte(attResp), name, time.Now())
	if err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}
	f.auth.AddCredential(f.cred)
	f.appendPasskey(t, *pk)
	return pk
}

func (f *ceremonyFixture) appendPasskey(t *testing.T, pk model.Passkey) {
	t.Helper()
	pks, err := model.ParsePasskeys(f.user.Authn)
	if err != nil {
		t.Fatalf("ParsePasskeys: %v", err)
	}
	// Replace-or-append by ID so a re-persisted (counter-updated) key is updated.
	replaced := false
	for i := range pks {
		if pks[i].ID == pk.ID {
			pks[i] = pk
			replaced = true
			break
		}
	}
	if !replaced {
		pks = append(pks, pk)
	}
	s, err := model.MarshalPasskeys(pks)
	if err != nil {
		t.Fatalf("MarshalPasskeys: %v", err)
	}
	f.user.Authn = s
}

// discoverableHandler resolves an assertion back to the fixture user, mimicking
// the W2 credentialID/userHandle → user lookup.
func (f *ceremonyFixture) discoverableHandler(t *testing.T) webauthn.DiscoverableUserHandler {
	return func(rawID, userHandle []byte) (webauthn.User, error) {
		id, ok := model.UserIDFromWebAuthnHandle(userHandle)
		if !ok || id != f.user.ID {
			t.Fatalf("handler got unexpected handle: %v", userHandle)
		}
		return f.user, nil
	}
}

// login runs a discoverable-login ceremony against w (which may differ from the
// fixture's own instance, e.g. a wrong-RP instance for negative tests). It bumps
// the virtual authenticator counter to counter before signing.
func (f *ceremonyFixture) login(t *testing.T, w *webauthn.WebAuthn, rp virtualwebauthn.RelyingParty, counter uint32) (webauthn.User, *webauthn.Credential, error) {
	t.Helper()
	assertion, session, err := BeginDiscoverableLogin(w)
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	const cid = "login-challenge"
	f.store.Put(cid, session, time.Minute)

	optsJSON, err := json.Marshal(assertion)
	if err != nil {
		t.Fatalf("marshal assertion: %v", err)
	}
	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(optsJSON))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	f.cred.Counter = counter
	asResp := virtualwebauthn.CreateAssertionResponse(rp, f.auth, f.cred, *asOpts)

	sess, ok := f.store.Take(cid)
	if !ok {
		t.Fatal("login challenge should be present")
	}
	return FinishDiscoverableLogin(w, *sess, []byte(asResp), f.discoverableHandler(t))
}

func TestCeremony_RegisterThenLogin(t *testing.T) {
	f := newCeremonyFixture(t)
	pk := f.register(t, "My Laptop")

	if pk.Name != "My Laptop" {
		t.Fatalf("name not stored: %q", pk.Name)
	}
	if pk.ID != base64.RawURLEncoding.EncodeToString(f.cred.ID) {
		t.Fatalf("stored credential id mismatch")
	}
	// Persisted passkey round-trips through the Authn column.
	pks, err := model.ParsePasskeys(f.user.Authn)
	if err != nil || len(pks) != 1 {
		t.Fatalf("expected 1 stored passkey, got %d err=%v", len(pks), err)
	}

	// First login: counter advances 0 -> 1.
	user, cred, err := f.login(t, f.w, f.rp, 1)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if user.(*model.User).ID != f.user.ID {
		t.Fatalf("login resolved wrong user")
	}
	if cred.Authenticator.SignCount != 1 {
		t.Fatalf("counter not advanced: %d", cred.Authenticator.SignCount)
	}

	// Persist the advanced counter, then a second login at 2 also succeeds.
	updated := pks[0]
	updated.SignCount = cred.Authenticator.SignCount
	f.appendPasskey(t, updated)

	if _, _, err := f.login(t, f.w, f.rp, 2); err != nil {
		t.Fatalf("second login should succeed: %v", err)
	}
}

func TestCeremony_CounterRollbackRejected(t *testing.T) {
	f := newCeremonyFixture(t)
	f.register(t, "Key")

	// Advance to counter 5 and persist it as the stored value.
	_, cred, err := f.login(t, f.w, f.rp, 5)
	if err != nil {
		t.Fatalf("initial login: %v", err)
	}
	pks, _ := model.ParsePasskeys(f.user.Authn)
	upd := pks[0]
	upd.SignCount = cred.Authenticator.SignCount // 5
	f.appendPasskey(t, upd)

	// Replay an older counter (3 < 5): clone signal -> rejected.
	if _, _, err := f.login(t, f.w, f.rp, 3); err != ErrClonedAuthenticator {
		t.Fatalf("counter rollback should be rejected with ErrClonedAuthenticator, got %v", err)
	}
	// Equal counter (5 == 5) is also a stall -> rejected.
	if _, _, err := f.login(t, f.w, f.rp, 5); err != ErrClonedAuthenticator {
		t.Fatalf("counter stall should be rejected, got %v", err)
	}
}

func TestCeremony_ChallengeReplayRejected(t *testing.T) {
	f := newCeremonyFixture(t)
	f.register(t, "Key")

	assertion, session, err := BeginDiscoverableLogin(f.w)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	const cid = "replay"
	f.store.Put(cid, session, time.Minute)

	optsJSON, _ := json.Marshal(assertion)
	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(optsJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f.cred.Counter = 1
	asResp := virtualwebauthn.CreateAssertionResponse(f.rp, f.auth, f.cred, *asOpts)

	// First redemption of the challenge succeeds.
	sess, ok := f.store.Take(cid)
	if !ok {
		t.Fatal("challenge missing")
	}
	if _, _, err := FinishDiscoverableLogin(f.w, *sess, []byte(asResp), f.discoverableHandler(t)); err != nil {
		t.Fatalf("first redemption should succeed: %v", err)
	}
	// The challenge is single-use: a replay finds nothing in the store.
	if _, ok := f.store.Take(cid); ok {
		t.Fatal("challenge must not be redeemable twice")
	}
}

func TestCeremony_WrongRPRejected(t *testing.T) {
	f := newCeremonyFixture(t)
	f.register(t, "Key")

	// The authenticator signs for evil.com's RP ID/origin, but the RP verifies
	// against example.com: the assertion must be rejected.
	wrongRP := virtualwebauthn.RelyingParty{ID: "evil.com", Name: "Evil", Origin: testWrongRP}
	if _, _, err := f.login(t, f.w, wrongRP, 1); err == nil {
		t.Fatal("assertion signed for a different RP must be rejected")
	}
}

func TestCeremony_WrongOriginRejected(t *testing.T) {
	f := newCeremonyFixture(t)
	f.register(t, "Key")

	// Correct RP ID but the client reports a foreign origin: rejected by origin
	// matching.
	foreignOriginRP := virtualwebauthn.RelyingParty{ID: testRPID, Name: "Example", Origin: "https://phish.example.com"}
	if _, _, err := f.login(t, f.w, foreignOriginRP, 1); err == nil {
		t.Fatal("assertion with a foreign origin must be rejected")
	}
}
