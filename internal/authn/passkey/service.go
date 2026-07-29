package passkey

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pkg/errors"
)

// Service is the host-side wiring around the pure passkey engine: it resolves the
// Relying Party from settings, owns the in-flight ceremony store, performs the
// credential→user reverse lookup with an ownership assertion, and persists the
// advanced sign counter. HTTP handlers (registration/management) and the built-in
// AuthNMethod adapter (login) share ONE Service so both flows use the same
// challenge store and RP resolution.
type Service struct {
	store *ceremonyStore
}

// Default is the process-wide passkey service. Both the built-in AuthNMethod
// adapter (RegisterBuiltin) and the HTTP handlers reference it, so a login
// ceremony begun through one path can be finished through the same store.
var Default = NewService()

// NewService builds an empty service with its own ceremony store.
func NewService() *Service {
	return &Service{store: newCeremonyStore(time.Now)}
}

// challengeTTL bounds how long a begun ceremony may stay redeemable. Short enough
// to blunt replay/opportunistic reuse, long enough for a user to complete the
// authenticator gesture.
const challengeTTL = 5 * time.Minute

// ErrChallengeNotFound is returned when a finish references a challenge id that
// is unknown or already redeemed/expired. It is a clean rejection (a replay or a
// timed-out ceremony), never an operational fault.
var ErrChallengeNotFound = errors.New("passkey: challenge not found or already used")

// BeginRegister starts a registration ceremony for an already-authenticated user
// (adding a passkey). requestBaseURL is the caller's best guess at the public
// base URL, used only when settings/site_url do not pin the RP. It returns an
// opaque challenge id (correlates the eventual finish) and the creation options
// the browser feeds to navigator.credentials.create.
func (s *Service) BeginRegister(user webauthn.User, requestBaseURL string) (string, *protocol.CredentialCreation, error) {
	rp, err := resolveRP(requestBaseURL)
	if err != nil {
		return "", nil, err
	}
	w, err := New(rp)
	if err != nil {
		return "", nil, err
	}
	creation, session, err := BeginRegistration(w, user)
	if err != nil {
		return "", nil, err
	}
	id := random.String(32)
	s.store.put(id, session, rp, challengeTTL)
	return id, creation, nil
}

// FinishRegister verifies a registration response against the stored challenge
// and returns a ready-to-persist model.Passkey (the caller appends it to the
// user's Authn column and persists). The RP is taken from the stored ceremony so
// verification uses the exact config Begin used.
func (s *Service) FinishRegister(user webauthn.User, challengeID string, responseJSON []byte, name string) (*model.Passkey, error) {
	p, ok := s.store.take(challengeID)
	if !ok {
		return nil, ErrChallengeNotFound
	}
	w, err := New(p.rp)
	if err != nil {
		return nil, err
	}
	return FinishRegistration(w, user, *p.session, responseJSON, name, time.Now())
}

// BeginLogin starts a passwordless (discoverable) login ceremony. It returns an
// opaque challenge id and the request options the browser feeds to
// navigator.credentials.get. requestBaseURL is used only when settings/site_url
// do not pin the RP.
func (s *Service) BeginLogin(requestBaseURL string) (string, *protocol.CredentialAssertion, error) {
	rp, err := resolveRP(requestBaseURL)
	if err != nil {
		return "", nil, err
	}
	w, err := New(rp)
	if err != nil {
		return "", nil, err
	}
	assertion, session, err := BeginDiscoverableLogin(w)
	if err != nil {
		return "", nil, err
	}
	id := random.String(32)
	s.store.put(id, session, rp, challengeTTL)
	return id, assertion, nil
}

// FinishLogin verifies a discoverable-login assertion against the stored
// challenge, resolves the owning user (asserting credential ownership),
// persists the advanced sign counter + last-used timestamp, and returns the
// resolved user. Every failure — replayed/expired challenge, bad assertion, a
// clone-signalling counter, or an ownership mismatch — surfaces as an error the
// caller treats as a clean credential rejection.
func (s *Service) FinishLogin(challengeID string, responseJSON []byte) (*model.User, error) {
	p, ok := s.store.take(challengeID)
	if !ok {
		return nil, ErrChallengeNotFound
	}
	w, err := New(p.rp)
	if err != nil {
		return nil, err
	}
	user, cred, err := FinishDiscoverableLogin(w, *p.session, responseJSON, discoverableHandler)
	if err != nil {
		return nil, err
	}
	mu, ok := user.(*model.User)
	if !ok {
		return nil, errors.Errorf("passkey: resolved user has unexpected type %T", user)
	}
	if err := persistLoginCounter(mu, cred, time.Now()); err != nil {
		return nil, err
	}
	return mu, nil
}

// discoverableHandler is the webauthn.DiscoverableUserHandler backing login: it
// maps an assertion's (rawID, userHandle) to the owning model.User.
//
// It resolves the user by the userHandle (the reversible 8-byte user id), then
// asserts that the resolved user actually OWNS the asserted credential id. That
// ownership check is the load-bearing guard: userHandle is client-supplied and
// not covered by the assertion signature, so a forged handle pointing at another
// account (e.g. an admin) is rejected here because that account's Authn does not
// contain this rawID. (go-webauthn independently matches rawID against the
// returned user's credentials, so this is defense in depth, not the only line.)
//
// This walks only the single resolved user's credential list — no full user-table
// scan is needed because userHandle already names the candidate account. If a
// future model ever divorces userHandle from the user id, fall back to a scan
// over op.GetUsers here (the seam the spec flags for a credential index table).
func discoverableHandler(rawID, userHandle []byte) (webauthn.User, error) {
	uid, ok := model.UserIDFromWebAuthnHandle(userHandle)
	if !ok {
		return nil, errors.New("passkey: malformed user handle")
	}
	user, err := op.GetUserById(uid)
	if err != nil {
		return nil, errors.Wrap(err, "passkey: user handle does not resolve")
	}
	want := base64.RawURLEncoding.EncodeToString(rawID)
	pks, err := model.ParsePasskeys(user.Authn)
	if err != nil {
		return nil, errors.Wrap(err, "passkey: cannot parse credentials")
	}
	for i := range pks {
		if pks[i].ID == want {
			return user, nil
		}
	}
	return nil, errors.New("passkey: credential not owned by the resolved user")
}

// persistLoginCounter writes the freshly observed sign counter and last-used
// timestamp back onto the matching stored passkey and persists the user. The
// counter must advance for anti-clone detection to keep working, so persisting it
// is a security-relevant step, not a nicety.
func persistLoginCounter(user *model.User, cred *webauthn.Credential, now time.Time) error {
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	pks, err := model.ParsePasskeys(user.Authn)
	if err != nil {
		return err
	}
	found := false
	for i := range pks {
		if pks[i].ID == id {
			pks[i].SignCount = cred.Authenticator.SignCount
			pks[i].LastUsedAt = now
			found = true
			break
		}
	}
	if !found {
		return errors.New("passkey: credential vanished before counter writeback")
	}
	marshaled, err := model.MarshalPasskeys(pks)
	if err != nil {
		return err
	}
	user.Authn = marshaled
	return op.UpdateUser(user)
}

// resolveRP builds the Relying Party config from settings, falling back to the
// derived base URL. Priority per field: explicit setting → conf.SiteURL →
// requestBaseURL. The origins/RP-ID must be stable across a ceremony's begin and
// finish; because the resolved RP is stored with the ceremony (ceremonyStore),
// finish never needs the request context again.
func resolveRP(requestBaseURL string) (RP, error) {
	rpID := strings.TrimSpace(setting.GetStr(conf.WebAuthnRPID))
	display := strings.TrimSpace(setting.GetStr(conf.WebAuthnRPDisplayName))
	origins := splitOrigins(setting.GetStr(conf.WebAuthnRPOrigins))

	// Prefer the operator-configured, stable public URL; fall back to what the
	// request implies only when site_url is not an absolute URL.
	base := strings.TrimSpace(conf.Conf.SiteURL)
	if !strings.HasPrefix(base, "http") {
		base = strings.TrimSpace(requestBaseURL)
	}

	if len(origins) == 0 {
		o, err := originOf(base)
		if err != nil {
			return RP{}, fmt.Errorf("passkey: cannot determine RP origin (set %s or site_url): %w", conf.WebAuthnRPOrigins, err)
		}
		origins = []string{o}
	}
	if rpID == "" {
		h, err := originHost(origins[0])
		if err != nil {
			return RP{}, fmt.Errorf("passkey: cannot derive RP ID from origin %q: %w", origins[0], err)
		}
		rpID = h
	}
	if display == "" {
		display = strings.TrimSpace(setting.GetStr(conf.SiteTitle))
	}
	if display == "" {
		display = "AList"
	}
	return RP{ID: rpID, DisplayName: display, Origins: origins}, nil
}

// splitOrigins parses the webauthn_rp_origins setting: a comma / newline /
// whitespace separated list, tolerant of blank entries.
func splitOrigins(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// originOf reduces a URL to its WebAuthn origin: scheme://host[:port], no path.
// This is exactly what a browser reports as the ceremony origin, so it must carry
// no trailing path or slash.
func originOf(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("empty url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("no scheme/host in %q", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ceremonyStore holds in-flight ceremonies keyed by an opaque challenge id. Each
// entry pins the SessionData AND the RP config used at Begin, so Finish rebuilds
// an identical *webauthn.WebAuthn regardless of how the RP was derived. Entries
// are single-use (take deletes) and expire after a TTL — the anti-replay spine.
// It mirrors the pure engine's ChallengeStore semantics but carries the extra RP
// so request-derived ceremonies stay consistent across begin/finish.
type ceremonyStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]pendingCeremony
}

type pendingCeremony struct {
	session *webauthn.SessionData
	rp      RP
	expires time.Time
}

func newCeremonyStore(now func() time.Time) *ceremonyStore {
	return &ceremonyStore{now: now, m: make(map[string]pendingCeremony)}
}

func (s *ceremonyStore) put(id string, session *webauthn.SessionData, rp RP, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = pendingCeremony{session: session, rp: rp, expires: s.now().Add(ttl)}
}

// take returns and removes the ceremony for id (single use). A missing or expired
// id returns (zero, false); an expired entry is evicted.
func (s *ceremonyStore) take(id string) (pendingCeremony, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return pendingCeremony{}, false
	}
	delete(s.m, id)
	if s.now().After(e.expires) {
		return pendingCeremony{}, false
	}
	return e, true
}
