package extension

import (
	"context"
	"fmt"
	"sync"

	"github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// AssistProvider fulfils one of an extension's declared assist forms (SDK
// §4.1): a field-level "assist" completion flow (`x-assist` on a JSONSchema
// field references an AssistForm; submitting it refills the originating
// field). It is the host-side seam for Assist, mirroring ActionInvoker for
// menu actions: built-in / in-process extensions register a concrete
// provider; gRPC-backed assist execution is a future track, so a declared
// assist form whose extension has no registered provider returns
// NOT_IMPLEMENT.
type AssistProvider interface {
	Assist(ctx context.Context, formID string, input map[string]any) (any, error)
}

var (
	assistMu        sync.RWMutex
	assistProviders = map[string]AssistProvider{}
)

// RegisterAssistProvider wires an in-process provider for an extension id. A
// nil provider deregisters. Safe for concurrent use.
func RegisterAssistProvider(extID string, p AssistProvider) {
	assistMu.Lock()
	defer assistMu.Unlock()
	if p == nil {
		delete(assistProviders, extID)
		return
	}
	assistProviders[extID] = p
}

// ErrAssistNotDeclared is returned when an extension exists but did not
// declare the requested form in its Contributions.AssistForms. Callers map it
// to 404: an undeclared form is indistinguishable from a missing one to a
// client.
var ErrAssistNotDeclared = fmt.Errorf("extension: assist form not declared")

// findAssistForm returns the declared AssistForm with id formID, or nil if
// the manifest declares no such form. SubmitAssist's "declared?" gate (its
// nil check) doubles as what used to be a separate assistDeclared bool
// helper — kept as one lookup since the caller needs the form's Schema too.
func findAssistForm(m ext.Manifest, formID string) *ext.AssistForm {
	for i := range m.Contributions.AssistForms {
		if m.Contributions.AssistForms[i].ID == formID {
			return &m.Contributions.AssistForms[i]
		}
	}
	return nil
}

// ErrAssistInvalidInput wraps a schema-validation failure of an assist
// form's submitted input against its declared AssistForm.Schema. Callers map
// it to 400: the request reached a real, declared form, but the payload
// itself is malformed — distinct from ErrAssistNotDeclared's 404.
var ErrAssistInvalidInput = fmt.Errorf("extension: assist input failed schema validation")

// assistFactory is the gRPC-dispatch seam for Assist (assist gRPC leg, Task
// 2), mirroring actionFactory (actions.go): nil until
// server/handles/plugins.go wires it to pluginproc.Default.AcquireAssist.
var assistFactory func(e *Entry) (ext.Assist, func(), error)

// SetAssistFactory installs the gRPC assist provider.
func SetAssistFactory(fn func(e *Entry) (ext.Assist, func(), error)) {
	assistFactory = fn
}

// resolveAssist resolves the gRPC ext.Assist for a declared-but-unwired-in-
// process entry, mirroring resolveAction — with one deliberate difference:
// it does NOT check the entry's declared Kinds. A menu action only exists on
// a Kind=Action entry, but an assist form can be declared by an entry of ANY
// Kind (a StorageDriver's config wizard, an AuthProvider's setup form, ...) —
// findAssistForm (Contributions.AssistForms membership) is already the gate
// that gets checked before resolveAssist is ever reached, so requiring a
// specific Kind here would be wrong, not just redundant. What resolveAssist
// does still require is that the plugin actually mounted the "assist"
// dispense name: an ExecGRPC entry whose impl never implements ext.Assist
// fails at Dispense time (host.LaunchAssist), which the assistFactory
// surfaces as an error here — same nil-return, fall-through-to-501 path as
// any other resolution failure.
func resolveAssist(e *Entry) (ext.Assist, func()) {
	if e.execMode == ExecGRPC && e.binPath != "" && assistFactory != nil {
		a, release, err := assistFactory(e)
		if err == nil {
			return a, release
		}
		log.Warnf("extension: resolveAssist %q: assistFactory failed: %v", e.Manifest.ID, err)
	}
	return nil, func() {}
}

// SubmitAssist routes an assist-form submission to an extension's handler,
// but only if the enabled extension declared that form. Resolution order:
//   - extension not enabled/registered      → ErrAssistNotDeclared (404)
//   - form not in AssistForms               → ErrAssistNotDeclared (404)
//   - declared, input fails Schema          → ErrAssistInvalidInput (400)
//   - declared, in-process provider         → provider result
//   - declared, no provider, gRPC resolves  → ext.Assist.Assist result
//   - declared, neither resolves            → ext NOT_IMPLEMENT (seam)
//
// The schema check runs before either dispatch path (in-process provider or
// gRPC), so a plugin's Assist implementation — in-process or across the
// wire — never has to re-validate what its own manifest already declared as
// required; a malformed submission is rejected at the host boundary instead
// of reaching plugin code at all. A form with no declared Schema (nil) skips
// the check entirely, unchanged from before.
//
// principal is the caller's identity handle, threaded through to the gRPC
// leg's Assist call (ext.Assist.Assist takes one); the in-process
// AssistProvider seam predates principal-carrying and is left unchanged
// (mirrors ActionInvoker, which never saw one either — InvokeExtensionAction
// doc comment).
func SubmitAssist(ctx context.Context, r *Registry, principal ext.Principal, extID, formID string, input map[string]any) (any, error) {
	e, ok := r.Get(extID)
	if !ok || !e.Enabled() {
		return nil, ErrAssistNotDeclared
	}
	form := findAssistForm(e.Manifest, formID)
	if form == nil {
		return nil, ErrAssistNotDeclared
	}
	if form.Schema != nil {
		if err := validateAgainstSchema(form.Schema, input); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrAssistInvalidInput, err)
		}
	}
	assistMu.RLock()
	p := assistProviders[extID]
	assistMu.RUnlock()
	if p != nil {
		return p.Assist(ctx, formID, input)
	}
	if a, release := resolveAssist(e); a != nil {
		defer release()
		return a.Assist(ctx, principal, formID, input)
	}
	return nil, ext.NewError(ext.ErrCodeNotImplement,
		fmt.Sprintf("assist form %q of extension %q has no host-side provider wired", formID, extID))
}
