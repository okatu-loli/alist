package extension

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlistGo/alist-ext"
	"github.com/google/uuid"
)

// DefaultWizardTTL bounds how long an idle install-wizard session lives. Kept
// short: a wizard is a foreground admin flow, and sessions hold only in-memory
// answers (no DB), swept lazily on the next engine touch.
const DefaultWizardTTL = 15 * time.Minute

// Sentinel wizard errors. Handlers map SessionNotFound/Expired to 404/410-ish.
var (
	ErrWizardSessionNotFound = fmt.Errorf("extension: wizard session not found")
	ErrWizardSessionExpired  = fmt.Errorf("extension: wizard session expired")
	ErrWizardNoSteps         = fmt.Errorf("extension: wizard has no steps")
	// ErrOAuthStateInvalid is returned by HandleOAuthCallback when state is
	// missing, unknown, expired, or does not match the session's current oauth
	// step (WS-8 anti-CSRF / anti-replay guard: state is single-use and is
	// deleted the moment a callback is looked up, whether or not the rest of
	// the callback succeeds).
	ErrOAuthStateInvalid = fmt.Errorf("extension: invalid or expired oauth state")
)

// WizardProber runs a `test` step's connectivity/validation probe for an
// extension. It is optional (SDK §4): if an extension registers none, test
// steps auto-pass with a note. config is the answers gathered so far.
type WizardProber interface {
	Probe(ctx context.Context, probeID string, config map[string]any) error
}

var (
	proberMu sync.RWMutex
	probers  = map[string]WizardProber{}
)

// RegisterWizardProber wires a test-step prober for an extension id. A nil
// prober deregisters.
func RegisterWizardProber(extID string, p WizardProber) {
	proberMu.Lock()
	defer proberMu.Unlock()
	if p == nil {
		delete(probers, extID)
		return
	}
	probers[extID] = p
}

func getProber(extID string) WizardProber {
	proberMu.RLock()
	defer proberMu.RUnlock()
	return probers[extID]
}

// WizardStepView is the client-facing shape of one wizard turn: the step to
// render (nil when done), a terminal flag, and — when a step re-prompts — an
// error, plus an informational note (e.g. a test step's result).
type WizardStepView struct {
	SessionID string          `json:"session_id"`
	Step      *ext.WizardStep `json:"step,omitempty"`
	Done      bool            `json:"done"`
	Error     string          `json:"error,omitempty"`
	Note      string          `json:"note,omitempty"`
	// AuthURL is set when the current step is an oauth step awaiting
	// authorization: the frontend opens it (e.g. in a popup) and waits for the
	// provider to redirect the browser to the host's oauth callback (WS-8).
	AuthURL string `json:"auth_url,omitempty"`
	// Answers carries the current step's own recorded answer (e.g. the oauth
	// step's exchanged token, once HandleOAuthCallback has completed it) so a
	// polling caller can pick it up without waiting for Done. Done responses
	// carry every step's merged answers instead (the full config, credentials
	// included, for the caller to use when creating the resulting storage).
	Answers map[string]any `json:"answers,omitempty"`
}

type wizardSession struct {
	id      string
	extID   string
	steps   []ext.WizardStep
	idx     int
	answers map[string]map[string]any
	expires time.Time

	// oauthStepID/oauthState/oauthURL cache the in-flight oauth request for the
	// CURRENT step (WS-8), so a poll that re-presents the same not-yet-completed
	// oauth step reuses the already-minted state/AuthURL instead of calling the
	// provider's AuthURL again on every poll. Cleared once the step completes.
	oauthStepID string
	oauthState  string
	oauthURL    string
}

// oauthPending is one in-flight AuthURL request (WS-8): the state the host
// minted, and enough to route/validate the callback that echoes it back —
// which session/step it belongs to, which AuthProvider to Exchange against,
// and the exact redirect_uri AuthURL used (Exchange must match it). Entries
// are single-use: HandleOAuthCallback deletes the entry the moment it looks
// one up, whether or not the rest of the callback succeeds.
type oauthPending struct {
	sessionID   string
	stepID      string
	provider    string
	redirectURI string
	expires     time.Time
}

// WizardEngine runs declarative install flows as step state machines. Sessions
// live in memory with a TTL. It is safe for concurrent use.
type WizardEngine struct {
	ttl          time.Duration
	mu           sync.Mutex
	sessions     map[string]*wizardSession
	oauthPending map[string]*oauthPending
}

// NewWizardEngine builds an engine with the given session TTL (<=0 uses the
// default).
func NewWizardEngine(ttl time.Duration) *WizardEngine {
	if ttl <= 0 {
		ttl = DefaultWizardTTL
	}
	return &WizardEngine{ttl: ttl, sessions: map[string]*wizardSession{}, oauthPending: map[string]*oauthPending{}}
}

// DefaultWizardEngine is the process-wide engine the HTTP handlers drive.
var DefaultWizardEngine = NewWizardEngine(DefaultWizardTTL)

// Start opens a session for extID over the given declared steps and returns
// the first step. steps is typically manifest.Wizard.Steps resolved by the
// caller. oauthRedirectURI is the host's own OAuth callback URL (WS-8); it is
// only used if/when the flow lands on an oauth step, so callers whose flow
// never has one may pass "".
func (e *WizardEngine) Start(ctx context.Context, extID string, steps []ext.WizardStep, oauthRedirectURI string) (WizardStepView, error) {
	if len(steps) == 0 {
		return WizardStepView{}, ErrWizardNoSteps
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()
	s := &wizardSession{
		id:      uuid.NewString(),
		extID:   extID,
		steps:   steps,
		idx:     0,
		answers: map[string]map[string]any{},
		expires: time.Now().Add(e.ttl),
	}
	e.sessions[s.id] = s
	return e.presentCtx(ctx, s, "", "", oauthRedirectURI), nil
}

// Next processes the current step's answers and advances. A validation failure
// (bad form answer, missing consent, failed probe) re-prompts the SAME step
// with an Error and does not advance. An oauth step that has not yet been
// completed by HandleOAuthCallback re-prompts itself with a "waiting" Note
// (poll again); once the callback has recorded a token, Next advances past it
// like any other step. oauthRedirectURI is threaded through in case the step
// being (re-)presented is an oauth step that has not minted its AuthURL yet.
func (e *WizardEngine) Next(ctx context.Context, sessionID string, answers map[string]any, oauthRedirectURI string) (WizardStepView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Look the target session up BEFORE sweeping so an expired one is reported
	// as Expired (not swept away and then reported as NotFound).
	s, ok := e.sessions[sessionID]
	if !ok {
		return WizardStepView{}, ErrWizardSessionNotFound
	}
	if time.Now().After(s.expires) {
		delete(e.sessions, sessionID)
		return WizardStepView{}, ErrWizardSessionExpired
	}
	e.sweepLocked()
	s.expires = time.Now().Add(e.ttl)

	step := s.steps[s.idx]
	note := ""
	nextIdx := s.idx + 1

	switch step.Type {
	case ext.WizardInfo:
		// render-only node.
	case ext.WizardRisk:
		// Step 11: a 🔴 risk interstitial is NOT skippable. It requires an
		// explicit checkbox (acknowledged) AND a second confirmation (confirm) —
		// two distinct affirmatives, so a single stray click cannot install an
		// untrusted plugin.
		ack, _ := answers["acknowledged"].(bool)
		confirm, _ := answers["confirm"].(bool)
		if !ack || !confirm {
			return e.presentCtx(ctx, s, "you must acknowledge the risks and confirm to install this plugin", "", oauthRedirectURI), nil
		}
		s.answers[step.ID] = map[string]any{"acknowledged": true, "confirm": true}
	case ext.WizardPermissionConsent:
		if consent, _ := answers["consent"].(bool); !consent {
			return e.presentCtx(ctx, s, "consent is required to continue", "", oauthRedirectURI), nil
		}
		// Record the WHOLE consent answer, not just the boolean (design §6.4). A
		// wizard-driven install submits the admin's approved selection — the
		// host_apis/network/fs the host-rendered consent UI (driven by the GET
		// /permissions endpoint, not the plugin) gathered — under "granted"; it
		// rides out in mergedAnswers so the install flow forwards it to SetGrant,
		// making the runtime authority reflect what was APPROVED, not what the
		// Manifest declared. The step here only captures the approval; the host
		// owns rendering the permission list so a plugin cannot forge it.
		rec := map[string]any{"consent": true}
		if g, ok := answers["granted"]; ok {
			rec["granted"] = g
		}
		s.answers[step.ID] = rec
	case ext.WizardForm:
		if err := validateAgainstSchema(step.Schema, answers); err != nil {
			return e.presentCtx(ctx, s, err.Error(), "", oauthRedirectURI), nil
		}
		s.answers[step.ID] = answers
	case ext.WizardChoice:
		chosen, _ := answers["option"].(string)
		opt := findOption(step.Options, chosen)
		if opt == nil {
			return e.presentCtx(ctx, s, fmt.Sprintf("unknown choice %q", chosen), "", oauthRedirectURI), nil
		}
		s.answers[step.ID] = map[string]any{"option": chosen}
		if opt.NextStep != "" {
			ji := indexOfStep(s.steps, opt.NextStep)
			if ji < 0 {
				return e.presentCtx(ctx, s, fmt.Sprintf("choice targets unknown step %q", opt.NextStep), "", oauthRedirectURI), nil
			}
			nextIdx = ji
		}
	case ext.WizardTest:
		if p := getProber(s.extID); p != nil {
			if err := p.Probe(ctx, step.ProbeID, s.mergedAnswers()); err != nil {
				return e.presentCtx(ctx, s, fmt.Sprintf("probe failed: %v", err), "", oauthRedirectURI), nil
			}
			note = "probe passed"
		} else {
			note = "no probe registered; auto-passed"
		}
	case ext.WizardOAuth:
		// A completed oauth step (HandleOAuthCallback already recorded a token
		// as this step's answer) advances like any other step. Otherwise
		// re-prompt the SAME step so a polling caller can keep checking; this
		// also (re-)mints the AuthURL on first entry (WS-8, no external broker —
		// the AuthProvider is called directly, in-process or over its own gRPC
		// subprocess).
		if !stepCompleted(s, step.ID) {
			return e.presentCtx(ctx, s, "", "waiting for oauth authorization to complete", oauthRedirectURI), nil
		}
		note = "oauth authorization completed"
	case ext.WizardDone:
		delete(e.sessions, s.id)
		return WizardStepView{SessionID: s.id, Done: true, Answers: s.mergedAnswers()}, nil
	default:
		return e.presentCtx(ctx, s, fmt.Sprintf("unknown step type %q", step.Type), "", oauthRedirectURI), nil
	}

	if nextIdx >= len(s.steps) {
		delete(e.sessions, s.id)
		return WizardStepView{SessionID: s.id, Done: true, Note: note, Answers: s.mergedAnswers()}, nil
	}
	s.idx = nextIdx
	return e.presentCtx(ctx, s, "", note, oauthRedirectURI), nil
}

// HandleOAuthCallback completes an in-flight oauth step (WS-8): it validates
// state against the pending AuthURL request that minted it, resolves the
// AuthProvider, and exchanges the code for a token by calling Exchange
// directly — in-process or over the provider's own gRPC subprocess, never
// through an external broker. The exchanged token is recorded as the step's
// answer, but the session is NOT advanced past the step here: the caller
// (typically the wizard's own browser tab, once it learns the popup reached
// this callback) still drives forward progress through Next, which — seeing
// the step's answer already carries a completed token — advances without
// re-invoking Exchange. state is single-use: it is deleted the moment it is
// looked up, whether or not the rest of the callback succeeds, so a replayed
// callback is always rejected.
func (e *WizardEngine) HandleOAuthCallback(ctx context.Context, code, state string) (WizardStepView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepLocked()

	pending, ok := e.oauthPending[state]
	if ok {
		delete(e.oauthPending, state)
	}
	if !ok || state == "" || time.Now().After(pending.expires) {
		return WizardStepView{}, ErrOAuthStateInvalid
	}

	s, ok := e.sessions[pending.sessionID]
	if !ok {
		return WizardStepView{}, ErrWizardSessionNotFound
	}
	if time.Now().After(s.expires) {
		delete(e.sessions, s.id)
		return WizardStepView{}, ErrWizardSessionExpired
	}
	if s.idx >= len(s.steps) || s.steps[s.idx].ID != pending.stepID || s.steps[s.idx].Type != ext.WizardOAuth {
		// The session has moved on (or was never on this step) since the
		// AuthURL was minted: a stale or replayed callback.
		return WizardStepView{}, ErrOAuthStateInvalid
	}

	provider, release, err := DefaultRegistry.GetAuthProvider(pending.provider)
	if err != nil {
		return WizardStepView{}, fmt.Errorf("extension: oauth callback: resolve provider %q: %w", pending.provider, err)
	}
	defer release()
	tok, err := provider.Exchange(ctx, ext.ExchangeRequest{Code: code, RedirectURI: pending.redirectURI})
	if err != nil {
		return WizardStepView{}, fmt.Errorf("extension: oauth exchange failed: %w", err)
	}

	s.expires = time.Now().Add(e.ttl)
	s.answers[pending.stepID] = map[string]any{
		"_completed": true,
		"token":      tokenToAnswer(tok),
	}
	if s.oauthStepID == pending.stepID {
		s.oauthStepID, s.oauthState, s.oauthURL = "", "", ""
	}
	return e.presentCtx(ctx, s, "", "oauth authorization completed", pending.redirectURI), nil
}

// presentCtx renders the session's current step. A `done` step is terminal
// (Done set), but the session is kept until Next confirms it, so the client
// can show the final screen. When the current step is a not-yet-completed
// oauth step and errMsg=="" (i.e. this isn't itself a re-prompt for some other
// failure), it mints — or, on a repeat poll of the same step, reuses — the
// AuthURL (WS-8).
func (e *WizardEngine) presentCtx(ctx context.Context, s *wizardSession, errMsg, note, oauthRedirectURI string) WizardStepView {
	step := s.steps[s.idx]
	view := WizardStepView{SessionID: s.id, Step: &step, Error: errMsg, Note: note}
	if step.Type == ext.WizardDone {
		// Reaching the done step (even before Next processes it as its own
		// turn) is terminal for the caller's purposes: hand back every step's
		// merged answers now — the full config, oauth token included — rather
		// than making the caller submit one more empty Next just to receive it.
		view.Done = true
		view.Answers = s.mergedAnswers()
	} else if a, ok := s.answers[step.ID]; ok {
		view.Answers = a
	}
	if step.Type == ext.WizardOAuth && errMsg == "" && !stepCompleted(s, step.ID) {
		if s.oauthStepID == step.ID && s.oauthState != "" {
			view.AuthURL = s.oauthURL
		} else if url, err := e.beginOAuthLocked(ctx, s, step, oauthRedirectURI); err != nil {
			view.Error = fmt.Sprintf("oauth: %v", err)
		} else {
			view.AuthURL = url
		}
	}
	return view
}

// beginOAuthLocked mints a fresh anti-CSRF state, asks step.Provider's
// AuthProvider for the vendor authorization URL, and records the pending
// request so HandleOAuthCallback can find it. Caller must hold e.mu.
func (e *WizardEngine) beginOAuthLocked(ctx context.Context, s *wizardSession, step ext.WizardStep, redirectURI string) (string, error) {
	if step.Provider == "" {
		return "", fmt.Errorf("oauth step %q declares no provider", step.ID)
	}
	if redirectURI == "" {
		return "", fmt.Errorf("no oauth redirect URI configured (set site_url)")
	}
	provider, release, err := DefaultRegistry.GetAuthProvider(step.Provider)
	if err != nil {
		return "", err
	}
	defer release()
	state, err := newOAuthState()
	if err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	url, err := provider.AuthURL(ctx, ext.AuthURLRequest{State: state, RedirectURI: redirectURI, Scopes: step.Scopes})
	if err != nil {
		return "", err
	}
	e.oauthPending[state] = &oauthPending{
		sessionID:   s.id,
		stepID:      step.ID,
		provider:    step.Provider,
		redirectURI: redirectURI,
		expires:     time.Now().Add(e.ttl),
	}
	s.oauthStepID, s.oauthState, s.oauthURL = step.ID, state, url
	return url, nil
}

// newOAuthState mints a random anti-CSRF/anti-replay token for an oauth step.
func newOAuthState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// stepCompleted reports whether stepID's recorded answer carries the
// "_completed" marker HandleOAuthCallback writes once Exchange succeeds.
func stepCompleted(s *wizardSession, stepID string) bool {
	a, ok := s.answers[stepID]
	if !ok {
		return false
	}
	done, _ := a["_completed"].(bool)
	return done
}

// tokenToAnswer flattens an ext.Token into a plain map[string]any (a JSON
// round trip) so it can sit in a wizardSession's answers next to ordinary form
// answers and travel out through WizardStepView.Answers/mergedAnswers.
func tokenToAnswer(tok ext.Token) map[string]any {
	b, err := json.Marshal(tok)
	if err != nil {
		return map[string]any{"access_token": tok.AccessToken}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{"access_token": tok.AccessToken}
	}
	return m
}

func (s *wizardSession) mergedAnswers() map[string]any {
	out := map[string]any{}
	for _, a := range s.answers {
		for k, v := range a {
			out[k] = v
		}
	}
	return out
}

func (e *WizardEngine) sweepLocked() {
	now := time.Now()
	for id, s := range e.sessions {
		if now.After(s.expires) {
			delete(e.sessions, id)
		}
	}
	for state, p := range e.oauthPending {
		if now.After(p.expires) {
			delete(e.oauthPending, state)
			continue
		}
		if _, ok := e.sessions[p.sessionID]; !ok {
			delete(e.oauthPending, state)
		}
	}
}

func findOption(opts []ext.WizardOption, id string) *ext.WizardOption {
	for i := range opts {
		if opts[i].ID == id {
			return &opts[i]
		}
	}
	return nil
}

func indexOfStep(steps []ext.WizardStep, id string) int {
	for i := range steps {
		if steps[i].ID == id {
			return i
		}
	}
	return -1
}

// validateAgainstSchema checks answers against a minimal JSON-Schema object:
// required presence, per-property primitive type, and enum membership. A nil
// schema accepts anything. It returns the first violation, so a re-prompt can
// name the offending field.
func validateAgainstSchema(schema *ext.JSONSchema, answers map[string]any) error {
	if schema == nil {
		return nil
	}
	for _, req := range schema.Required {
		v, ok := answers[req]
		if !ok || v == nil || v == "" {
			return fmt.Errorf("field %q is required", req)
		}
	}
	for name, prop := range schema.Properties {
		v, ok := answers[name]
		if !ok || v == nil {
			continue // absence handled by Required above
		}
		if err := checkType(name, prop, v); err != nil {
			return err
		}
		if len(prop.Enum) > 0 {
			sv := fmt.Sprintf("%v", v)
			if !containsStr(prop.Enum, sv) {
				return fmt.Errorf("field %q must be one of %s", name, strings.Join(prop.Enum, ", "))
			}
		}
	}
	return nil
}

func checkType(name string, prop *ext.JSONSchema, v any) error {
	switch prop.Type {
	case "", "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("field %q must be a string", name)
		}
	case "number", "integer":
		if _, ok := v.(float64); !ok { // JSON numbers decode to float64
			return fmt.Errorf("field %q must be a number", name)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("field %q must be a boolean", name)
		}
	}
	return nil
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// validateWizardOAuthProviders checks, for every WizardOAuth step in m.Wizard,
// that step.Provider names an extension that both EXISTS and declares
// Kind=AuthProvider (WS-8: an oauth step drives a live ext.AuthProvider
// directly — never through an external broker — so a step referencing a
// nonexistent or wrong-Kind extension would only fail at runtime, deep inside
// beginOAuthLocked, with a far less actionable error). lookup resolves any id
// OTHER than m.ID; a step referencing the declaring manifest's OWN id is
// checked against m directly (via hasKind) with no registry lookup at all, so
// a plugin that is itself an AuthProvider driving its own wizard is never
// blocked by registration ordering (it doesn't need to already be registered
// to reference itself).
//
// Callers pass a lookup bound to however they can safely read the registry at
// their call site. Register, RegisterPluginMode, AND Enable all call this
// WHILE ALREADY HOLDING r.mu (Register/RegisterPluginMode fixed in review
// round 1: checking with r.Get — its own RLock — BEFORE taking r.mu left a
// TOCTOU window where a concurrent Deregister of the referenced provider,
// racing between the check and the r.byID insert, could let a now-dangling
// reference slip in registered), so every caller passes the lock-free
// r.getLocked rather than the exported, self-locking r.Get (calling r.Get
// while r.mu is already held would deadlock against Go's non-reentrant
// sync.RWMutex).
func validateWizardOAuthProviders(m ext.Manifest, lookup func(id string) (*Entry, bool)) error {
	for _, step := range m.Wizard.Steps {
		if step.Type != ext.WizardOAuth {
			continue
		}
		if step.Provider == "" {
			return fmt.Errorf("extension %q: wizard oauth step %q declares no provider", m.ID, step.ID)
		}
		if step.Provider == m.ID {
			if !hasKind(m, ext.KindAuthProvider) {
				return fmt.Errorf("extension %q: wizard oauth step %q references itself as provider but does not declare kind %q",
					m.ID, step.ID, ext.KindAuthProvider)
			}
			continue
		}
		e, ok := lookup(step.Provider)
		if !ok {
			return fmt.Errorf("extension %q: wizard oauth step %q references unknown provider extension %q",
				m.ID, step.ID, step.Provider)
		}
		if !e.DeclaresKind(ext.KindAuthProvider) {
			return fmt.Errorf("extension %q: wizard oauth step %q references provider %q which does not declare kind %q",
				m.ID, step.ID, step.Provider, ext.KindAuthProvider)
		}
	}
	return nil
}

// RiskStepID is the id of the synthesized risk interstitial.
const RiskStepID = "risk"

// BuildWizardSteps prepares the wizard step sequence for an install of a
// package classified as tier. It strips any manifest-declared risk steps (a
// plugin cannot supply — or opt out of — its own risk copy), then prepends a
// synthesized, non-skippable risk step for every non-official tier: 🟡 gets a
// source-attribution warning naming the imported source that vouched for it,
// 🔴 gets the full unreviewed-plugin warning. 🟢 gets none.
func BuildWizardSteps(declared []ext.WizardStep, tier Tier, source string, man ext.Manifest) []ext.WizardStep {
	steps := make([]ext.WizardStep, 0, len(declared)+1)
	for _, s := range declared {
		if s.Type == ext.WizardRisk {
			continue // strip plugin-declared risk steps; the host owns this copy
		}
		steps = append(steps, s)
	}
	switch tier {
	case TierOfficial:
		return steps
	case TierVerified:
		return append([]ext.WizardStep{verifiedRiskStep(source, man)}, steps...)
	default:
		return append([]ext.WizardStep{thirdPartyRiskStep(man)}, steps...)
	}
}

// verifiedRiskStep is the 🟡 interstitial: the package is vouched for by an
// admin-imported source, but is not first-party and the official project has
// not audited it.
func verifiedRiskStep(source string, man ext.Manifest) ext.WizardStep {
	return ext.WizardStep{
		ID:   RiskStepID,
		Type: ext.WizardRisk,
		Title: map[string]string{
			"en": "Install a source-verified plugin?",
			"zh": "安装来源认证的插件？",
		},
		Body: map[string]string{
			"en": "This plugin is verified by the imported source \"" + source + "\", not by the official project, " +
				"and the official project has not audited its code. It will run as a separate process with the " +
				"permissions you grant.\nRequested permissions: " + permissionSummary(man.Permissions) + ".\n" +
				"Install only if you trust this source.",
			"zh": "此插件由你导入的插件源「" + source + "」认证，并非官方维护，官方未审计其代码。" +
				"它将以独立进程运行，持有你授予的权限。\n申请的权限：" + permissionSummary(man.Permissions) + "。\n" +
				"请仅在信任该来源时安装。",
		},
	}
}

// thirdPartyRiskStep is the 🔴 interstitial. Since plugin parity there is no
// sandbox to fall back on, so the copy states plainly that the plugin runs as
// a full-permission process and can reach everything the admin grants,
// including storage credentials.
func thirdPartyRiskStep(man ext.Manifest) ext.WizardStep {
	return ext.WizardStep{
		ID:   RiskStepID,
		Type: ext.WizardRisk,
		Title: map[string]string{
			"en": "Install an unreviewed plugin?",
			"zh": "安装未经审核的插件？",
		},
		Body: map[string]string{
			"en": thirdPartyRiskBodyEN(man),
			"zh": thirdPartyRiskBodyZH(man),
		},
	}
}

func thirdPartyRiskBodyEN(man ext.Manifest) string {
	var b strings.Builder
	b.WriteString("This plugin is THIRD-PARTY and UNREVIEWED. ")
	b.WriteString("License: " + provOrUnknown(man.Provenance.License) + "; ")
	b.WriteString("Pricing: " + provOrUnknown(man.Provenance.Pricing) + "; ")
	b.WriteString("Source: " + provOrUnknown(man.Provenance.SourceURL) + ".\n")
	b.WriteString("Requested permissions: " + permissionSummary(man.Permissions) + ".\n")
	b.WriteString("It will run as a separate process with the FULL permissions you grant — ")
	b.WriteString("it can access everything you allow, including storage credentials. ")
	b.WriteString("The official project cannot audit it. Install only if you trust its source.")
	return b.String()
}

func thirdPartyRiskBodyZH(man ext.Manifest) string {
	var b strings.Builder
	b.WriteString("此插件为第三方，未经任何审核。")
	b.WriteString("许可证：" + provOrUnknown(man.Provenance.License) + "；")
	b.WriteString("定价：" + provOrUnknown(man.Provenance.Pricing) + "；")
	b.WriteString("源码：" + provOrUnknown(man.Provenance.SourceURL) + "。\n")
	b.WriteString("申请的权限：" + permissionSummary(man.Permissions) + "。\n")
	b.WriteString("它将以完整权限的独立进程运行，可以访问你授予的一切，包括存储凭证。")
	b.WriteString("官方无法审计此插件。请仅在信任其来源时安装。")
	return b.String()
}

func provOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func permissionSummary(p ext.Permissions) string {
	var parts []string
	if len(p.Network) > 0 {
		parts = append(parts, "network="+strings.Join(p.Network, ","))
	}
	if len(p.StorageTokens) > 0 {
		parts = append(parts, "storage_tokens="+strings.Join(p.StorageTokens, ","))
	}
	if p.Filesystem != "" && p.Filesystem != "none" {
		parts = append(parts, "filesystem="+p.Filesystem)
	}
	if len(p.HostAPIs) > 0 {
		parts = append(parts, "host_apis="+strings.Join(p.HostAPIs, ","))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}
