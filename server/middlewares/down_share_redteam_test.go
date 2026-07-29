package middlewares

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/common"
)

// Attack surface 2 (share red-team): the /d sign channel.
//
// ShareDownload confines a ?sub= inside the share subtree, then 302-redirects to
// /d/<in-subtree path>?sign=common.SignPath(<that path>). The download itself is
// served by middlewares.Down, which is the SAME unauthenticated channel /d has
// always exposed. Two questions decide whether share can leak a file outside the
// grant through that channel:
//
//  1. Is the share-minted sign BOUND to the exact canonical path? If it is, an
//     attacker who captures a legit /d link cannot pivot it to another file by
//     rewriting the path while keeping the sign — Down re-resolves the ref of the
//     REQUEST path and verifies the sign against that, so a mismatched path fails
//     closed (401). TestShareDSignBoundToPath pins this.
//
//  2. When the instance does NOT enforce download signing (SignAll off, storage
//     EnableSign off, no meta password), needSign returns false and /d serves the
//     path with no signature at all. That is alist's PRE-EXISTING open-download
//     posture — it is identical with or without the share feature, because the
//     path is reachable via a hand-typed /d URL regardless of any share. Share
//     neither weakens nor strengthens it: it only ever SignPaths an in-subtree
//     file (never a foreign one), so it introduces no new reachable path.
//     TestNeedSignOpenPostureIsPreExisting documents that posture.

var shareRedteamSeq int64

// mustShareLikeStorage builds a Local storage with a "share_root" subtree (the
// part a share would grant) and a sibling "outside" subtree that no share covers,
// so a /d path-tamper has a real out-of-subtree target to try to reach.
func mustShareLikeStorage(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&shareRedteamSeq, 1)
	mount := fmt.Sprintf("/rt%d", n)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "share_root"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "share_root", "file.txt"), []byte("granted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside", "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mount,
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	})
	if err != nil {
		t.Fatalf("create storage %s: %v", mount, err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return mount
}

// TestShareDSignBoundToPath is the load-bearing proof that the /d channel cannot
// be pivoted off the shared file. It mints the sign EXACTLY as ShareDownload does
// (common.SignPath of the in-subtree file) with download signing enforced
// (mustSignAllMeta), then replays that same sign against out-of-subtree paths.
//
// Because the sign is over the ResourceRef Canonical() of the specific path and
// Down re-derives the canonical of the REQUEST path before sign.Verify, every
// pivot must be aborted (401). If the sign were NOT path-bound (e.g. a global
// "any download is fine" token), the tamper cases would pass and a share would
// leak /outside/secret.txt — so this test failing loudly means the /d hand-off
// stopped confining downloads.
func TestShareDSignBoundToPath(t *testing.T) {
	mount := mustShareLikeStorage(t)
	mustSignAllMeta(t) // model an instance that DOES enforce download signing

	sharedFile := mount + "/share_root/file.txt"
	// The share layer signs exactly the confined path — nothing broader.
	shareSign := common.SignPath(sharedFile)

	// (a) Legit hand-off: the signed in-subtree file downloads fine.
	if c := runDown(t, sharedFile, shareSign); c.IsAborted() {
		t.Fatalf("legit share /d link for %s must pass Down, got aborted", sharedFile)
	}

	// (b) Path pivot to a sibling OUTSIDE the subtree, reusing the share's sign.
	pivots := map[string]string{
		"sibling file":     mount + "/outside/secret.txt",
		"sibling dir":      mount + "/outside",
		"traversal pivot":  mount + "/share_root/../outside/secret.txt", // parsePath cleans the ".." away
		"root pivot":       mount,
		"other mount root": "/",
	}
	for name, tampered := range pivots {
		if c := runDown(t, tampered, shareSign); !c.IsAborted() {
			t.Fatalf("%s: share sign for %q must NOT verify for tampered path %q (sign not path-bound → /d escape)",
				name, sharedFile, tampered)
		}
	}

	// (c) Stripping the sign entirely, when signing is enforced, is refused too.
	if c := runDown(t, mount+"/outside/secret.txt", ""); !c.IsAborted() {
		t.Fatal("no sign with download signing enforced must be refused, not passed through")
	}
}

// TestNeedSignOpenPostureIsPreExisting documents (not laments) attack surface 2's
// second half: with download signing UNENFORCED — no SignAll, no storage
// EnableSign, no meta password — needSign is false and /d serves any path with no
// signature. This is alist's own open-download posture and is orthogonal to
// share: the exact same path is reachable by typing the /d URL by hand whether or
// not a share exists. Share never SignPaths a path outside the confined subtree
// (op.ShareSubPath runs first), so it adds no new reachable path here — it neither
// aggravates nor mitigates the instance's chosen download posture.
//
// If this flips (needSign true by default), that is a behavioral change to the
// whole /d channel, not to share, and this test will surface it.
func TestNeedSignOpenPostureIsPreExisting(t *testing.T) {
	// A bare path with no meta, SignAll off (default), and no storage claiming it
	// (so IsStorageSignEnabled is false) requires no sign.
	if needSign(nil, "/rt-unenforced/outside/secret.txt") {
		t.Fatal("precondition: with no meta / SignAll / EnableSign, needSign must be false " +
			"(this is the pre-existing open /d posture, independent of share)")
	}
	// A meta that exists but carries no password likewise imposes no sign.
	if needSign(&model.Meta{Path: "/rt-unenforced", Password: ""}, "/rt-unenforced/outside/secret.txt") {
		t.Fatal("a password-less meta must not require a sign")
	}
}
