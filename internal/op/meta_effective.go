package op

import (
	stdpath "path"
	"strings"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
)

// EffectiveMeta is the chain-merged view of every meta from "/" down to the
// target path, replacing the single-ancestor GetNearestMeta semantics
// (ancestor password now survives an empty descendant meta; hide rules union).
type EffectiveMeta struct {
	Password        string
	PasswordApplies bool
	HidePatterns    []string
	Write           bool
	Readme          string
	Header          string
}

func ancestorChain(reqPath string) []string {
	reqPath = utils.FixAndCleanPath(reqPath)
	chain := []string{"/"}
	if reqPath == "/" {
		return chain
	}
	cur := ""
	for _, part := range strings.Split(strings.TrimPrefix(reqPath, "/"), "/") {
		cur += "/" + part
		chain = append(chain, cur)
	}
	return chain
}

func ResolveEffectiveMeta(reqPath string) (*EffectiveMeta, error) {
	reqPath = utils.FixAndCleanPath(reqPath)
	em := &EffectiveMeta{}
	for _, p := range ancestorChain(reqPath) {
		m, err := getMetaByPath(p)
		if err != nil {
			if errors.Is(err, errs.MetaNotFound) || errors.Cause(err) == errs.MetaNotFound {
				continue
			}
			return nil, err
		}
		exact := utils.PathEqual(m.Path, reqPath)
		if m.Password != "" && (exact || m.PSub) {
			em.Password, em.PasswordApplies = m.Password, true // root→leaf: nearest wins
		}
		if m.Write && (exact || m.WSub) {
			em.Write = true
		}
		if m.Readme != "" && (exact || m.RSub) {
			em.Readme = m.Readme
		}
		if m.Header != "" && (exact || m.HeaderSub) {
			em.Header = m.Header
		}
	}
	patterns, err := EffectiveHidePatterns(stdpath.Dir(reqPath))
	if err != nil {
		return nil, err
	}
	em.HidePatterns = patterns
	return em, nil
}

// EffectiveHidePatterns returns every hide rule applying to the children of
// dirPath: metas at dirPath itself always apply, strict ancestors only with
// HSub (same IsApply semantics as the legacy check, but unioned over the
// whole chain instead of nearest-only).
func EffectiveHidePatterns(dirPath string) ([]string, error) {
	dirPath = utils.FixAndCleanPath(dirPath)
	var out []string
	for _, p := range ancestorChain(dirPath) {
		m, err := getMetaByPath(p)
		if err != nil {
			if errors.Is(err, errs.MetaNotFound) || errors.Cause(err) == errs.MetaNotFound {
				continue
			}
			return nil, err
		}
		if m.Hide == "" {
			continue
		}
		if utils.PathEqual(m.Path, dirPath) || m.HSub {
			for _, line := range strings.Split(m.Hide, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					out = append(out, line)
				}
			}
		}
	}
	return out, nil
}
