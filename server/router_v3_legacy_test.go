package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestLegacyAPILoginTOTPStepUpRouteRegistered is the red-team I6 regression:
// the legacy /api mount (kept alive one major release alongside /api/v4) must
// carry the /auth/login/2fa alias too — a client still pointed at /api that
// gets a step-up challenge from loginHash (shared by both mounts) needs a
// same-prefix endpoint to actually complete the login. This only pins ROUTE
// WIRING (reaches handles.LoginTOTPStepUp, not a 404 catch-all); the
// handler's own behavior is exhaustively covered in server/handles.
func TestLegacyAPILoginTOTPStepUpRouteRegistered(t *testing.T) {
	env := newV4Env(t)

	body, _ := json.Marshal(map[string]string{"challenge_id": "bogus", "code": "000000"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login/2fa", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	env.e.ServeHTTP(w, req)

	// A 404 here would mean the ROUTE itself is unwired (a catch-all miss); the
	// handler's own envelope-code 404 below (builtin.totp is never registered
	// in this narrow env, which doesn't run bootstrap.InitTOTP) is a DIFFERENT
	// thing — a normal, correctly-routed response — so only the transport
	// status is asserted against a literal 404 here.
	if w.Code == 404 {
		t.Fatalf("legacy /api/auth/login/2fa must be routed, got 404: %s", w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// This env never registers builtin.totp (no bootstrap.InitTOTP), so the
	// real handler correctly reaches the generalized second-factor dispatch
	// (finishSecondFactor, server/handles/authn.go) and reports "no such
	// method" — 404 — the SAME contract every other AuthNMethod endpoint uses
	// for an unregistered id. It must never mint a token either way.
	if resp.Code != 404 {
		t.Fatalf("bogus challenge against an unregistered method must be rejected (code=404), got code=%d body=%s", resp.Code, w.Body.String())
	}
}
