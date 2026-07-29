package op

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// 复用 relocate_test.go 的 mustCreateMeta(同包)。
func TestEffectiveMetaAncestorPasswordAndDescendantHide(t *testing.T) {
	mustCreateMeta(t, &model.Meta{Path: "/em/top", Password: "pw", PSub: true})
	mustCreateMeta(t, &model.Meta{Path: "/em/top/mid"}) // 空 meta,旧 GetNearestMeta 在此截断
	mustCreateMeta(t, &model.Meta{Path: "/em/top/mid/leaf", Hide: "secret.*", HSub: true})

	em, err := ResolveEffectiveMeta("/em/top/mid/deep")
	if err != nil {
		t.Fatal(err)
	}
	if !em.PasswordApplies || em.Password != "pw" {
		t.Fatalf("ancestor password must apply through empty meta: %+v", em)
	}
	// hide 规则作用于 leaf 的子项
	patterns, err := EffectiveHidePatterns("/em/top/mid/leaf/sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 1 || patterns[0] != "secret.*" {
		t.Fatalf("HSub hide must reach descendants: %v", patterns)
	}
	// 不带 HSub 的 hide 只作用于自己那层
	if p, _ := EffectiveHidePatterns("/em/top"); len(p) != 0 {
		t.Fatalf("hide must not leak upward: %v", p)
	}
}

func TestEffectiveMetaWriteAndPSubBoundary(t *testing.T) {
	mustCreateMeta(t, &model.Meta{Path: "/em2/w", Write: true, WSub: false})
	mustCreateMeta(t, &model.Meta{Path: "/em2/pw", Password: "x", PSub: false})

	if em, _ := ResolveEffectiveMeta("/em2/w"); !em.Write {
		t.Fatal("exact write must apply")
	}
	if em, _ := ResolveEffectiveMeta("/em2/w/sub"); em.Write {
		t.Fatal("write without WSub must not inherit")
	}
	if em, _ := ResolveEffectiveMeta("/em2/pw/sub"); em.PasswordApplies {
		t.Fatal("password without PSub must not inherit")
	}
	if em, _ := ResolveEffectiveMeta("/em2/pw"); !em.PasswordApplies || em.Password != "x" {
		t.Fatal("password exact must apply")
	}
}

func TestEffectiveMetaNearestPasswordWins(t *testing.T) {
	mustCreateMeta(t, &model.Meta{Path: "/em3", Password: "outer", PSub: true})
	mustCreateMeta(t, &model.Meta{Path: "/em3/in", Password: "inner", PSub: true})
	if em, _ := ResolveEffectiveMeta("/em3/in/x"); em.Password != "inner" {
		t.Fatalf("nearest applying password wins: %+v", em)
	}
}

func TestEffectiveMetaReadmeHeaderChainMerge(t *testing.T) {
	mustCreateMeta(t, &model.Meta{Path: "/em4/top", Readme: "top-readme", RSub: true, Header: "top-header", HeaderSub: false})
	mustCreateMeta(t, &model.Meta{Path: "/em4/top/mid"}) // 空 meta,不截断 readme 沿链

	// readme 沿 RSub 传到子孙
	if em, _ := ResolveEffectiveMeta("/em4/top/mid/leaf"); em.Readme != "top-readme" {
		t.Fatalf("RSub readme must reach descendants: %+v", em)
	}
	// header 无 HeaderSub,不传到子孙
	if em, _ := ResolveEffectiveMeta("/em4/top/mid/leaf"); em.Header != "" {
		t.Fatalf("header without HeaderSub must not inherit: %+v", em)
	}
	// 精确匹配自身总是生效,不论 *Sub
	if em, _ := ResolveEffectiveMeta("/em4/top"); em.Readme != "top-readme" || em.Header != "top-header" {
		t.Fatalf("exact path must apply readme/header regardless of sub flags: %+v", em)
	}
}

func TestEffectiveMetaReadmeNearestWins(t *testing.T) {
	// 独立子树,避免上一测试对 "not found" 的负缓存污染这里新建的 meta 路径。
	mustCreateMeta(t, &model.Meta{Path: "/em5/top", Readme: "top-readme", RSub: true})
	mustCreateMeta(t, &model.Meta{Path: "/em5/top/leaf", Readme: "leaf-readme", RSub: true})
	if em, _ := ResolveEffectiveMeta("/em5/top/leaf/deep"); em.Readme != "leaf-readme" {
		t.Fatalf("nearest applying readme wins: %+v", em)
	}
}
