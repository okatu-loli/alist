package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// Proactive cap-token renewal tests (design cap-renewal). The property under
// test throughout: a copy that runs longer than one capability token's TTL
// must keep working on EITHER transport, by the copy executor itself asking
// for a fresh token (CapClient.RenewCap) well before the current one's
// remaining life runs out — rather than depending on an external mechanism
// (like the outbound heartbeat loop's TaskToken updates) that reverse mode has
// no equivalent of. See task.go's "proactive cap-token renewal" section doc.

// fakeCapToken builds a token shaped exactly like agentsrv.IssueTaskCap's
// output (base64url(JSON claims) + "." + sig) but with a throwaway signature
// — capTokenExpiry never verifies the signature (it can't; this module holds
// no signing key), it only peeks at the claimed expiry, so a fake signature
// is fine for exercising that parse path.
func fakeCapToken(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Exp int64 `json:"exp"`
	}{Exp: exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

func TestCapTokenExpiryParsesClaimedExpiry(t *testing.T) {
	want := time.Now().Add(5 * time.Minute)
	tok := fakeCapToken(t, want)

	got, ok := capTokenExpiry(tok)
	if !ok {
		t.Fatal("expected capTokenExpiry to parse a well-formed token")
	}
	if got.Unix() != want.Unix() {
		t.Fatalf("exp = %v, want %v", got, want)
	}
}

func TestCapTokenExpiryRejectsMalformed(t *testing.T) {
	for _, tok := range []string{"", "nodot", "..", "not-base64.sig", "tok"} {
		if _, ok := capTokenExpiry(tok); ok {
			t.Fatalf("capTokenExpiry(%q) must report ok=false", tok)
		}
	}
}

// TestWrapRenewingTokenRenewsNearExpiryToken pins the core "剩余寿命 <1/3"
// trigger: a token whose own claimed remaining life is comfortably under
// assumedCapTTL/3 must be renewed on the very first tokenFn() call — no fake
// clock/sleep needed, since the token's OWN claimed expiry is what drives the
// decision.
func TestWrapRenewingTokenRenewsNearExpiryToken(t *testing.T) {
	nearExpiry := fakeCapToken(t, time.Now().Add(1*time.Minute)) // << assumedCapTTL/3 (5m)
	cc := &recordingCapClient{renewToken: "renewed-tok", renewExpires: time.Now().Add(assumedCapTTL).Unix()}

	tokenFn := wrapRenewingToken(context.Background(), cc, "t1", StaticToken(nearExpiry))

	got := tokenFn()
	if cc.renewCapCalls != 1 {
		t.Fatalf("RenewCap calls = %d, want 1", cc.renewCapCalls)
	}
	if got != "renewed-tok" {
		t.Fatalf("tokenFn() = %q, want the renewed token", got)
	}
}

// TestWrapRenewingTokenSkipsRenewalFarFromExpiry: a token with a full, fresh
// TTL must NOT trigger a renewal call at all.
func TestWrapRenewingTokenSkipsRenewalFarFromExpiry(t *testing.T) {
	fresh := fakeCapToken(t, time.Now().Add(assumedCapTTL))
	cc := &recordingCapClient{renewToken: "should-not-be-used"}

	tokenFn := wrapRenewingToken(context.Background(), cc, "t1", StaticToken(fresh))

	got := tokenFn()
	if cc.renewCapCalls != 0 {
		t.Fatalf("RenewCap calls = %d, want 0 (token is far from expiry)", cc.renewCapCalls)
	}
	if got != fresh {
		t.Fatalf("tokenFn() = %q, want the original token unchanged", got)
	}
}

// TestWrapRenewingTokenBackstopMaxInterval pins the "或每 10 分钟" backstop:
// even a token this module can't parse an expiry out of (e.g. a plain opaque
// string, which is exactly what every pre-cap-renewal test in this package
// uses) is proactively renewed once capRenewMaxInterval has elapsed. The var
// is shrunk here (never in production) so the test doesn't wait real minutes.
func TestWrapRenewingTokenBackstopMaxInterval(t *testing.T) {
	oldInterval := capRenewMaxInterval
	oldBackoff := capRenewRetryBackoff
	capRenewMaxInterval = 20 * time.Millisecond
	capRenewRetryBackoff = 0
	t.Cleanup(func() {
		capRenewMaxInterval = oldInterval
		capRenewRetryBackoff = oldBackoff
	})

	cc := &recordingCapClient{renewToken: "renewed-by-backstop"}
	tokenFn := wrapRenewingToken(context.Background(), cc, "t1", StaticToken("opaque-tok"))

	if got := tokenFn(); got != "opaque-tok" {
		t.Fatalf("first call = %q, want the original token (interval not yet elapsed)", got)
	}
	if cc.renewCapCalls != 0 {
		t.Fatalf("RenewCap calls = %d, want 0 before the interval elapses", cc.renewCapCalls)
	}

	time.Sleep(30 * time.Millisecond)

	if got := tokenFn(); got != "renewed-by-backstop" {
		t.Fatalf("after the max interval elapsed, tokenFn() = %q, want the renewed token", got)
	}
	if cc.renewCapCalls != 1 {
		t.Fatalf("RenewCap calls = %d, want exactly 1", cc.renewCapCalls)
	}
}

// TestWrapRenewingTokenRenewalFailureKeepsOldTokenAndBacksOff pins "续期失败
// 不立刻毙任务": a RenewCap error must never surface to the caller — the OLD
// token keeps being served — and repeated near-expiry calls in quick
// succession must not hammer RenewCap past capRenewRetryBackoff.
func TestWrapRenewingTokenRenewalFailureKeepsOldTokenAndBacksOff(t *testing.T) {
	oldBackoff := capRenewRetryBackoff
	capRenewRetryBackoff = time.Hour // never elapses within this test
	t.Cleanup(func() { capRenewRetryBackoff = oldBackoff })

	nearExpiry := fakeCapToken(t, time.Now().Add(1*time.Minute))
	cc := &recordingCapClient{renewErr: errors.New("server unreachable")}
	tokenFn := wrapRenewingToken(context.Background(), cc, "t1", StaticToken(nearExpiry))

	for i := 0; i < 5; i++ {
		got := tokenFn()
		if got != nearExpiry {
			t.Fatalf("call %d: tokenFn() = %q, want the original token kept on renewal failure", i, got)
		}
	}
	if cc.renewCapCalls != 1 {
		t.Fatalf("RenewCap calls = %d, want exactly 1 (later calls must back off)", cc.renewCapCalls)
	}
}

// TestWrapRenewingTokenExternalUpdateWinsAndResetsClock pins that an external
// renewal (the caller's OWN tokenFn holder changing on its own — e.g. the
// outbound Runner's TaskToken picking up a heartbeat-renewed token) is always
// adopted immediately and takes priority over this wrapper's own bookkeeping,
// exactly like the pre-existing TestRunCopyTaskUsesRenewedTokenForNextCall
// behavior this wrapper must not regress.
func TestWrapRenewingTokenExternalUpdateWinsAndResetsClock(t *testing.T) {
	current := "external-v1"
	base := func() string { return current }
	cc := &recordingCapClient{renewToken: "should-not-be-used"}

	tokenFn := wrapRenewingToken(context.Background(), cc, "t1", base)

	if got := tokenFn(); got != "external-v1" {
		t.Fatalf("tokenFn() = %q, want external-v1", got)
	}

	current = "external-v2"
	if got := tokenFn(); got != "external-v2" {
		t.Fatalf("tokenFn() = %q, want the externally-updated external-v2", got)
	}
	if cc.renewCapCalls != 0 {
		t.Fatalf("RenewCap calls = %d, want 0 (external update alone must not trigger our own renewal)", cc.renewCapCalls)
	}
}

// TestRunCopyTaskWithProactivelyRenewsNearExpiryToken is the executor-level
// integration pin: RunCopyTaskWith must run its cap calls through the
// renewing wrapper, so a copy that starts with a near-expiry token
// transparently picks up a freshly renewed one for its LATER calls in the
// same run — this is the fix for reverse mode's gap (a dispatched copy used
// to capture one StaticToken for its entire run, see this file's top doc).
func TestRunCopyTaskWithProactivelyRenewsNearExpiryToken(t *testing.T) {
	body := "renew-integration-body"
	srv := srcServer(t, body)

	nearExpiry := fakeCapToken(t, time.Now().Add(1*time.Minute))
	cc := &recordingCapClient{
		srcLink:      SrcLink{URL: srv.URL, Size: int64(len(body))},
		renewToken:   "renewed-mid-copy",
		renewExpires: time.Now().Add(assumedCapTTL).Unix(),
	}

	err := RunCopyTaskWith(t.Context(), cc, Task{TaskID: "t-renew", Type: "copy", SrcPath: "/a/f.bin", DstDir: "/b"}, StaticToken(nearExpiry))
	if err != nil {
		t.Fatalf("RunCopyTaskWith: %v", err)
	}

	if cc.renewCapCalls == 0 {
		t.Fatal("expected RunCopyTaskWith to have proactively renewed the near-expiry token")
	}
	// The initial running-report is sent BEFORE any renewal check has a
	// chance to run (tokenFn() is called for the very first time exactly
	// there), so — since a near-expiry token renews on its FIRST call — every
	// call including the first must already observe the renewed token here:
	// wrapRenewingToken's renewal happens synchronously inside the very first
	// tokenFn() invocation.
	if cc.statusTokens[0] != "renewed-mid-copy" {
		t.Fatalf("initial running-report token = %q, want the token renewed on first use", cc.statusTokens[0])
	}
	if cc.srcLinkToken != "renewed-mid-copy" {
		t.Fatalf("src-link token = %q, want renewed-mid-copy", cc.srcLinkToken)
	}
	if cc.putToken != "renewed-mid-copy" {
		t.Fatalf("put-content token = %q, want renewed-mid-copy", cc.putToken)
	}
	if cc.statuses[len(cc.statuses)-1] != proto.TaskStatusDone {
		t.Fatalf("final status = %q, want done", cc.statuses[len(cc.statuses)-1])
	}
}
