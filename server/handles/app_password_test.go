package handles

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// App password self-service handler tests (design W3). These pin the same
// two properties the share handler tests pin for its own resource: creator
// (here: owner) isolation on delete, and that credential material never
// leaks back through any DTO — CreateAppPasswordResp carries the plaintext
// exactly ONCE (the create response), never again on List.

func parseAppPasswordCreateResp(t *testing.T, w *httptest.ResponseRecorder) CreateAppPasswordResp {
	t.Helper()
	var env struct {
		Data CreateAppPasswordResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse create app password resp: %v", err)
	}
	return env.Data
}

func parseAppPasswordList(t *testing.T, w *httptest.ResponseRecorder) []AppPasswordDTO {
	t.Helper()
	var env struct {
		Data []AppPasswordDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse app password list: %v", err)
	}
	return env.Data
}

var appPasswordFixtureSeq int64

func appPasswordFixtureUser(t *testing.T) *model.User {
	t.Helper()
	n := atomic.AddInt64(&appPasswordFixtureSeq, 1)
	return &model.User{ID: 80000 + uint(n), Username: fmt.Sprintf("apwhandler%d", n), Role: model.Roles{model.GENERAL}}
}

// TestAppPasswordCreateListDelete drives the full CRUD surface: create
// returns a usable DTO plus the plaintext exactly once, list surfaces the
// created row without the plaintext, and delete removes it from a subsequent
// list.
func TestAppPasswordCreateListDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := appPasswordFixtureUser(t)

	// Create.
	w := driveJSON(user, "POST", `{"name":"phone client"}`, CreateAppPassword)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("create must be 200, got %d: %s", got, w.Body.String())
	}
	created := parseAppPasswordCreateResp(t, w)
	if created.Plaintext == "" || !strings.HasPrefix(created.Plaintext, "alp_") {
		t.Fatalf("create must return an alp_-prefixed plaintext, got %q", created.Plaintext)
	}
	if created.AppPassword.ID == 0 {
		t.Fatal("create must return a persisted id")
	}
	if created.AppPassword.Name != "phone client" {
		t.Fatalf("create must echo the name, got %q", created.AppPassword.Name)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "\"hash\"") || strings.Contains(strings.ToLower(w.Body.String()), "\"salt\"") {
		t.Fatalf("create response must never expose hash/salt fields: %s", w.Body.String())
	}

	// List: contains the created row, WITHOUT the plaintext anywhere, and
	// without hash/salt fields.
	w = driveNoArg(user, ListAppPasswords)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	if strings.Contains(w.Body.String(), created.Plaintext) {
		t.Fatalf("list must never leak the one-time plaintext: %s", w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "\"hash\"") || strings.Contains(strings.ToLower(w.Body.String()), "\"salt\"") {
		t.Fatalf("list response must never expose hash/salt fields: %s", w.Body.String())
	}
	list := parseAppPasswordList(t, w)
	var found bool
	for _, dto := range list {
		if dto.ID == created.AppPassword.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created app password missing from list: %s", w.Body.String())
	}

	// Delete: removes it from a subsequent list.
	w = driveDelete(user, created.AppPassword.ID, DeleteAppPassword)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("delete must be 200, got %d: %s", got, w.Body.String())
	}

	w = driveNoArg(user, ListAppPasswords)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list after delete must be 200, got %d: %s", got, w.Body.String())
	}
	for _, dto := range parseAppPasswordList(t, w) {
		if dto.ID == created.AppPassword.ID {
			t.Fatalf("deleted app password still present in list: %s", w.Body.String())
		}
	}
}

// TestAppPasswordDeleteCrossOwnerDenied pins ownership confinement: user B
// cannot delete user A's app password (unknown-shaped 404, never a silent
// success), and the row survives so A can still see it.
func TestAppPasswordDeleteCrossOwnerDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	userA := appPasswordFixtureUser(t)
	userB := appPasswordFixtureUser(t)

	w := driveJSON(userA, "POST", `{"name":"owned by A"}`, CreateAppPassword)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("create must be 200, got %d: %s", got, w.Body.String())
	}
	created := parseAppPasswordCreateResp(t, w)

	w = driveDelete(userB, created.AppPassword.ID, DeleteAppPassword)
	if got := respCode(t, w); got != 404 && got != 403 {
		t.Fatalf("cross-owner delete must be 404 or 403, got %d: %s", got, w.Body.String())
	}

	// Never actually deleted: A still sees it.
	w = driveNoArg(userA, ListAppPasswords)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	var found bool
	for _, dto := range parseAppPasswordList(t, w) {
		if dto.ID == created.AppPassword.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cross-owner delete attempt must never actually delete the row: %s", w.Body.String())
	}
}

// TestAppPasswordDeleteUnknownID pins the not-found shape for an id that
// never existed at all (same code path as cross-owner, uniform).
func TestAppPasswordDeleteUnknownID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := appPasswordFixtureUser(t)

	w := driveDelete(user, 99999999, DeleteAppPassword)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown id delete must be 404, got %d: %s", got, w.Body.String())
	}
}

// TestAppPasswordCreateLimitExceeded pins the 400 shape for the per-user cap
// (op.AppPasswordMaxPerUser), driven end to end through the HTTP handler.
func TestAppPasswordCreateLimitExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := appPasswordFixtureUser(t)

	for i := 0; i < 20; i++ {
		w := driveJSON(user, "POST", fmt.Sprintf(`{"name":"client-%d"}`, i), CreateAppPassword)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("create %d must be 200, got %d: %s", i, got, w.Body.String())
		}
	}
	w := driveJSON(user, "POST", `{"name":"one-too-many"}`, CreateAppPassword)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("21st create must be 400, got %d: %s", got, w.Body.String())
	}
}
