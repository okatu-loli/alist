package extension

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// Output budgets for a decorator group (design §2.6 gives a TIME budget; these
// bound SIZE, which the time budget does not — a fast-but-huge decorator could
// otherwise inflate one listing into an unbounded response + equal host memory,
// the F2 red-team MEDIUM finding). FileDecorator is not native-tier, so an
// untrusted/buggy decorator is in scope. A value over the per-value cap is
// dropped; a group over the total cap is truncated to what fit.
const (
	maxFieldValueBytes = 16 << 10 // 16 KiB per field value
	maxGroupBytes      = 1 << 20  // 1 MiB per group
)

// This file is the host consumption of Kind=FileDecorator (design §2.6): the
// backend of the "extension field groups, fetched on demand, under a per-group
// budget" protocol behind FileColumn contributions (label badges, share
// markers). The base fs/list always returns; a decorator group is computed only
// when a client asks for it (fields=_labels) and only over the page's refs, and
// each group runs under a time budget — a slow decorator's group is marked
// PENDING and the base listing returns immediately, so a misbehaving decorator
// only ever slows its own column, never the file listing (design §2.6, §4).

// DefaultFieldBudget is the per-group time budget (design §2.6: default 200ms).
// A group whose decorators do not all return within it is marked pending and
// backfilled later via the fs/fields endpoint.
const DefaultFieldBudget = 200 * time.Millisecond

// fileDecoratorFactory is the gRPC-dispatch seam (mirrors eventHookFactory).
var fileDecoratorFactory func(e *Entry) (ext.FileDecorator, func(), error)

// SetFileDecoratorFactory installs the gRPC FileDecorator provider.
func SetFileDecoratorFactory(fn func(e *Entry) (ext.FileDecorator, func(), error)) {
	fileDecoratorFactory = fn
}

var (
	fileDecoratorMu        sync.RWMutex
	fileDecoratorInstances = map[string]ext.FileDecorator{}
)

// RegisterFileDecoratorInstance registers an in-process ext.FileDecorator under
// id (a built-in/feature-bundle decorator or a test double). A nil value
// deregisters.
func RegisterFileDecoratorInstance(id string, d ext.FileDecorator) {
	fileDecoratorMu.Lock()
	defer fileDecoratorMu.Unlock()
	if d == nil {
		delete(fileDecoratorInstances, id)
		return
	}
	fileDecoratorInstances[id] = d
}

func resolveFileDecorator(e *Entry) (ext.FileDecorator, func()) {
	fileDecoratorMu.RLock()
	if d, ok := fileDecoratorInstances[e.Manifest.ID]; ok {
		fileDecoratorMu.RUnlock()
		return d, func() {}
	}
	fileDecoratorMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && fileDecoratorFactory != nil {
		if d, release, err := fileDecoratorFactory(e); err == nil {
			return d, release
		}
	}
	return nil, func() {}
}

// FieldResult is the decoration payload attached to an fs/list response (and
// returned by the fs/fields backfill endpoint): the ready field groups plus the
// groups that exceeded their budget and must be backfilled.
type FieldResult struct {
	// Fields maps group name → ref key (ext.RefKey) → the column value.
	Fields map[string]map[string]any `json:"fields,omitempty"`
	// Pending lists the requested groups that did not complete within budget; the
	// client re-requests them via GET /api/v4/fs/fields.
	Pending []string `json:"pending,omitempty"`
}

// resolvedDecorator pairs a decorator with the group it was matched for and its
// release func.
type resolvedDecorator struct {
	dec     ext.FileDecorator
	release func()
}

// decoratorsFor returns the enabled Kind=FileDecorator instances that declare
// group. The caller must release each.
func decoratorsFor(r *Registry, group string) []resolvedDecorator {
	var out []resolvedDecorator
	for _, e := range r.ByKind(ext.KindFileDecorator) {
		dec, release := resolveFileDecorator(e)
		if dec == nil {
			release()
			continue
		}
		if containsStr(dec.Groups(), group) {
			out = append(out, resolvedDecorator{dec: dec, release: release})
		} else {
			release()
		}
	}
	return out
}

// DecorateFields computes the requested extension field groups over refs, in
// parallel, under a per-group time budget. A group whose decorators all return
// within budget lands in Fields; a group that exceeds budget (or whose decorator
// errors) lands in Pending and is NOT waited on — the base listing never blocks
// on a slow decorator. refs MUST already be filtered to what the requesting user
// may see (the caller filters the listing first); a decorator is never handed a
// ref outside the user's view.
func DecorateFields(ctx context.Context, r *Registry, groups []string, refs []ext.ObjRef, budget time.Duration) FieldResult {
	if budget <= 0 {
		budget = DefaultFieldBudget
	}
	res := FieldResult{Fields: map[string]map[string]any{}}
	var mu sync.Mutex
	var wg sync.WaitGroup

	seen := map[string]bool{}
	for _, g := range groups {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		decs := decoratorsFor(r, g)
		if len(decs) == 0 {
			// No enabled decorator provides this group: nothing to compute, and it
			// is not pending (there is no one to backfill from).
			continue
		}
		wg.Add(1)
		go func(group string, decs []resolvedDecorator) {
			defer wg.Done()
			defer func() {
				for _, d := range decs {
					d.release()
				}
			}()
			merged, ok := runGroup(ctx, group, decs, refs, budget)
			mu.Lock()
			if !ok {
				res.Pending = append(res.Pending, group)
			} else if len(merged) > 0 {
				res.Fields[group] = merged
			}
			mu.Unlock()
		}(g, decs)
	}
	wg.Wait()

	sort.Strings(res.Pending)
	if len(res.Fields) == 0 {
		res.Fields = nil
	}
	return res
}

// runGroup runs every decorator matched for group under the shared budget and
// merges their results. It returns ok=false if any decorator errors or does not
// return within budget (the group is pending); the still-running decorator
// goroutine is abandoned (its ctx is cancelled) rather than waited on, so a slow
// decorator cannot extend the response past the budget.
func runGroup(ctx context.Context, group string, decs []resolvedDecorator, refs []ext.ObjRef, budget time.Duration) (map[string]any, bool) {
	gctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// The only keys a decorator may contribute are the refs it was actually
	// handed: a decorator returning a key for a page-external / other-user path
	// is a response-injection vector (F2 red-team Finding B), so intersect its
	// output with the requested set. This also bounds the key count to len(refs).
	allowed := make(map[string]struct{}, len(refs))
	for _, rf := range refs {
		allowed[ext.RefKey(rf)] = struct{}{}
	}

	type out struct {
		m   map[string]any
		err error
	}
	results := make([]chan out, len(decs))
	for i := range decs {
		ch := make(chan out, 1)
		results[i] = ch
		go func(d ext.FileDecorator, ch chan out) {
			defer func() {
				if rec := recover(); rec != nil {
					ch <- out{err: recoverErr(rec)}
				}
			}()
			m, err := d.Decorate(gctx, group, refs)
			ch <- out{m: m, err: err}
		}(decs[i].dec, ch)
	}

	merged := map[string]any{}
	total := 0
	for _, ch := range results {
		select {
		case o := <-ch:
			if o.err != nil {
				log.Warnf("extension: FileDecorator group %q error: %v", group, o.err)
				return nil, false
			}
			for k, v := range o.m {
				if _, ok := allowed[k]; !ok {
					continue // key outside the requested refs — dropped (Finding B)
				}
				b, err := json.Marshal(v)
				if err != nil || len(b) > maxFieldValueBytes {
					continue // unmarshalable or oversized value dropped (Finding A)
				}
				total += len(b) + len(k)
				if total > maxGroupBytes {
					log.Warnf("extension: FileDecorator group %q exceeded %d bytes, truncated", group, maxGroupBytes)
					return merged, true // bounded partial result
				}
				merged[k] = v
			}
		case <-gctx.Done():
			// Budget exceeded: mark pending and do not wait for the slow decorator.
			return nil, false
		}
	}
	return merged, true
}

// recoverErr normalizes a recovered panic value into an error.
func recoverErr(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return ext.NewError(ext.ErrCodeNotImplement, "file decorator panicked")
}
