package middlewares

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// setAdminTokenSetting persists conf.Token/conf.TokenPlaintext directly,
// bypassing the handlers.ResetToken/SetToken path (that lives in a
// different package) so adminTokenMatches — the exact branch touched by the
// hash/plaintext storage-mode split — can be exercised in isolation.
func setAdminTokenSetting(t *testing.T, value string, plaintext bool) {
	t.Helper()
	tokenItem := model.SettingItem{Key: conf.Token, Value: value, Type: conf.TypeString, Group: model.SINGLE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&tokenItem); err != nil {
		t.Fatal(err)
	}
	plaintextValue := "false"
	if plaintext {
		plaintextValue = "true"
	}
	plaintextItem := model.SettingItem{Key: conf.TokenPlaintext, Value: plaintextValue, Type: conf.TypeBool, Group: model.SINGLE, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(&plaintextItem); err != nil {
		t.Fatal(err)
	}
}

// TestAdminTokenMatchesPlaintextMode pins the unchanged branch: plaintext
// storage still does a straight constant-time comparison.
func TestAdminTokenMatchesPlaintextMode(t *testing.T) {
	setAdminTokenSetting(t, "plain-admin-token", true)
	if !adminTokenMatches("plain-admin-token") {
		t.Fatalf("plaintext mode: the correct token must match")
	}
	if adminTokenMatches("wrong-token") {
		t.Fatalf("plaintext mode: an incorrect token must not match")
	}
	if adminTokenMatches("") {
		t.Fatalf("plaintext mode: an empty Authorization header must not match")
	}
}

// TestAdminTokenMatchesHashedMode covers the new branch: hashed storage
// verifies via utils.VerifyToken.
func TestAdminTokenMatchesHashedMode(t *testing.T) {
	token := "hashed-admin-token-value"
	hashed, err := utils.HashToken(token)
	if err != nil {
		t.Fatal(err)
	}
	setAdminTokenSetting(t, hashed, false)

	if !adminTokenMatches(token) {
		t.Fatalf("hash mode: the correct token must verify and match")
	}
	if adminTokenMatches("wrong-token") {
		t.Fatalf("hash mode: an incorrect token must not match")
	}
	if adminTokenMatches("") {
		t.Fatalf("hash mode: an empty Authorization header must not match")
	}
}

// TestAdminTokenMatchesHashedModeRejectsStoredHashAsBearerValue: an internal
// caller that (incorrectly) forwarded the raw stored setting value as the
// bearer token — rather than the original plaintext — must NOT authenticate.
// Only the plaintext token that produced the hash verifies.
func TestAdminTokenMatchesHashedModeRejectsStoredHashAsBearerValue(t *testing.T) {
	token := "another-admin-token"
	hashed, err := utils.HashToken(token)
	if err != nil {
		t.Fatal(err)
	}
	setAdminTokenSetting(t, hashed, false)

	if adminTokenMatches(hashed) {
		t.Fatalf("the raw stored hash string must not itself authenticate as admin")
	}
}
