package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// Share authed-handler security tests (design §2.3-A / §3). The gates are
// authorization-sensitive, so each is driven end to end: creating a share
// requires ActRead on the target path (a share delegates the creator's read
// right, so no read means no share), listing returns only the caller's own
// shares, and deleting is confined to the creator (or an admin). The DTO must
// never carry PasswordHash — it is the one credential the public layer must not
// leak back through the creator surface either.

// driveNoArg runs a handler that takes no body and no params, just the caller in
// context (ListShares).
func driveNoArg(u *model.User, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// driveDelete runs DeleteShare with an :id path param and the caller in context.
func driveDelete(u *model.User, id uint, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/x", nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(int(id))}}
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

func parseShareDTO(t *testing.T, w *httptest.ResponseRecorder) ShareDTO {
	t.Helper()
	var env struct {
		Data ShareDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse share resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseShareList(t *testing.T, w *httptest.ResponseRecorder) []ShareDTO {
	t.Helper()
	var env struct {
		Data []ShareDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse share list: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestShareCreateAuthz pins the ActRead gate on creation: a caller without read
// on the target path cannot mint a share (403), and a caller with read gets a
// token whose public URL is /s/<token>.
func TestShareCreateAuthz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("no-read-403", func(t *testing.T) {
		u, _, mount := setupLabelFixture(t)
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q}`, mount+"/sec"), CreateShare)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("create without read must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("with-read-200-token", func(t *testing.T) {
		u, _, mount := setupLabelFixture(t)
		w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q}`, mount+"/vis"), CreateShare)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("create with read must be 200, got %d: %s", got, w.Body.String())
		}
		dto := parseShareDTO(t, w)
		if dto.Token == "" {
			t.Fatalf("create must return a token: %s", w.Body.String())
		}
		if dto.URL != "/s/"+dto.Token {
			t.Fatalf("public url must be /s/<token>, got %q", dto.URL)
		}
	})
}

// TestShareListOwnOnly pins creator isolation on listing: two users each mint a
// share, and A's listing shows A's token and never B's.
func TestShareListOwnOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, _, mount := setupLabelFixture(t)

	n := atomic.AddInt64(&labelFixtureSeq, 1)
	uidA := 70000 + uint(n)
	uidB := 71000 + uint(n)

	sA, err := op.CreateShare(context.Background(), uidA, mount+"/vis", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	sB, err := op.CreateShare(context.Background(), uidB, mount+"/sec", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	userA := &model.User{ID: uidA, Username: fmt.Sprintf("shareA%d", n), Role: model.Roles{model.GENERAL}}
	w := driveNoArg(userA, ListShares)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, sA.Token) {
		t.Fatalf("A's listing must contain A's own share token: %s", body)
	}
	if strings.Contains(body, sB.Token) {
		t.Fatalf("A's listing leaked B's share token (creator isolation broken): %s", body)
	}
}

// TestShareDeleteCreatorIsolation pins delete confinement: A cannot delete B's
// share (403, non-admin), A can delete its own, and an admin can delete anyone's.
func TestShareDeleteCreatorIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, _, mount := setupLabelFixture(t)

	n := atomic.AddInt64(&labelFixtureSeq, 1)
	uidA := 72000 + uint(n)
	uidB := 73000 + uint(n)
	userA := &model.User{ID: uidA, Username: fmt.Sprintf("delA%d", n), Role: model.Roles{model.GENERAL}}

	t.Run("cross-creator-403", func(t *testing.T) {
		sB, err := op.CreateShare(context.Background(), uidB, mount+"/sec", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		w := driveDelete(userA, sB.ID, DeleteShare)
		if got := respCode(t, w); got != 403 {
			t.Fatalf("deleting another user's share must be 403, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("own-ok", func(t *testing.T) {
		sA, err := op.CreateShare(context.Background(), uidA, mount+"/vis", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		w := driveDelete(userA, sA.ID, DeleteShare)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("deleting own share must be 200, got %d: %s", got, w.Body.String())
		}
	})

	t.Run("admin-any-ok", func(t *testing.T) {
		sB, err := op.CreateShare(context.Background(), uidB, mount+"/sec", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		admin := adminUser(t)
		w := driveDelete(admin, sB.ID, DeleteShare)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("admin deleting any share must be 200, got %d: %s", got, w.Body.String())
		}
	})
}

// TestShareListNoPasswordHash pins the credential-leak guard: a password-
// protected share surfaces needs_password=true but its raw PasswordHash never
// appears in the DTO wire form.
func TestShareListNoPasswordHash(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, _, mount := setupLabelFixture(t)

	n := atomic.AddInt64(&labelFixtureSeq, 1)
	uid := 74000 + uint(n)
	s, err := op.CreateShare(context.Background(), uid, mount+"/vis", "s3cr3t", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.PasswordHash == "" {
		t.Fatal("precondition: a password share must have a non-empty PasswordHash")
	}

	user := &model.User{ID: uid, Username: fmt.Sprintf("pw%d", n), Role: model.Roles{model.GENERAL}}
	w := driveNoArg(user, ListShares)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, s.PasswordHash) {
		t.Fatalf("share DTO leaked PasswordHash: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "passwordhash") || strings.Contains(body, "password_hash") {
		t.Fatalf("share DTO exposed a password-hash field: %s", body)
	}
	dtos := parseShareList(t, w)
	var found bool
	for _, d := range dtos {
		if d.Token == s.Token {
			found = true
			if !d.NeedsPassword {
				t.Fatalf("password share must report needs_password=true: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("own password share missing from listing: %s", body)
	}
}
