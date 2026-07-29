package op_test

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// TestDelUserCacheInvalidatesStaleEntry proves op.DelUserCache — the
// function cmd.DelUserCacheOnline now calls directly, in-process, instead of
// making an HTTP self-call to POST /api/admin/user/del_cache authenticated
// with conf.Token (broken once the admin API token is stored hashed by
// default; see cmd/user.go and pkg/utils.HashToken) — actually evicts a
// stale cached user so the next read reflects the current DB row.
func TestDelUserCacheInvalidatesStaleEntry(t *testing.T) {
	username := "cache-invalidate-user"
	u := &model.User{
		Username: username,
		Language: "en",
	}
	u.SetPassword("x")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.DeleteUserById(u.ID)
	})

	// Warm the cache.
	cached, err := op.GetUserByName(username)
	if err != nil {
		t.Fatalf("get user (warm cache): %v", err)
	}
	if cached.Language != "en" {
		t.Fatalf("unexpected initial Language: %q", cached.Language)
	}

	// Mutate the DB row directly, bypassing op.UpdateUser (which would itself
	// invalidate the cache) — this simulates the cache going stale.
	stale := *cached
	stale.Language = "zh-CN"
	if err := db.UpdateUser(&stale); err != nil {
		t.Fatalf("db update user: %v", err)
	}

	// Without a cache clear, the cache keeps serving the pre-mutation value.
	before, err := op.GetUserByName(username)
	if err != nil {
		t.Fatalf("get user (before del): %v", err)
	}
	if before.Language != "en" {
		t.Fatalf("expected stale cached Language %q, got %q", "en", before.Language)
	}

	if err := op.DelUserCache(username); err != nil {
		t.Fatalf("DelUserCache: %v", err)
	}

	after, err := op.GetUserByName(username)
	if err != nil {
		t.Fatalf("get user (after del): %v", err)
	}
	if after.Language != "zh-CN" {
		t.Fatalf("expected fresh Language %q after DelUserCache, got %q", "zh-CN", after.Language)
	}
}
