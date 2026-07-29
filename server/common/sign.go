package common

import (
	stdpath "path"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/sign"
)

func Sign(obj model.Obj, parent string, encrypt bool) string {
	if obj.IsDir() || (!encrypt && !setting.GetBool(conf.SignAll)) {
		return ""
	}
	return SignPath(stdpath.Join(parent, obj.GetName()))
}

// SignPath signs the ResourceRef canonical form of reqPath rather than the
// raw virtual path, so a signature survives a mount rename: it anchors to
// the storage and its relative path, not the mount point reqPath was built
// from. reqPath above every mount point (no storage owns it) has no ref to
// anchor to, so it falls back to signing the raw path string directly —
// still signable, just not rename-stable. Callers that verify a sign must
// resolve the same way (see server/middlewares/down.go) or signatures will
// never match.
func SignPath(reqPath string) string {
	ref, err := op.ResolvePathAnchor(reqPath)
	if err != nil {
		return sign.Sign(reqPath)
	}
	return sign.Sign(ref.Canonical())
}

// SignPathWithDuration is SignPath with a caller-specified TTL embedded in
// the signature (via sign.WithDuration) instead of the global
// conf.LinkExpiration setting SignPath/sign.Sign implicitly use — which
// defaults to 0 ("never expires"). Use this wherever a signed URL's lifetime
// must be bounded by something narrower and more specific than the
// server-wide default, e.g. a short-lived capability's own remaining life
// (see server/handles/agent_task.go's AgentTaskSrcLink proxied fallback).
// Resolves the SAME canonical form as SignPath (and, in turn, the same one
// server/middlewares/down.go's Down handler re-derives to verify), so a URL
// signed here still redeems at /d — it is simply now bounded, not unlimited.
func SignPathWithDuration(reqPath string, d time.Duration) string {
	ref, err := op.ResolvePathAnchor(reqPath)
	if err != nil {
		return sign.WithDuration(reqPath, d)
	}
	return sign.WithDuration(ref.Canonical(), d)
}
