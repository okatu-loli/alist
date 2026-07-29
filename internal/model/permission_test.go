// internal/model/permission_test.go
package model

import "testing"

func TestPermissionDefaults(t *testing.T) {
	p := Permission{Path: "/data", Actions: Actions{ActRead}, Effect: EffectAllow, Recursive: true}
	if p.Effect != "allow" || EffectDeny != "deny" {
		t.Errorf("effect constants wrong")
	}
	if OriginScope != "scope" || OriginManual != "manual" {
		t.Errorf("origin constants wrong")
	}
	if !p.Actions.Contains(ActRead) {
		t.Errorf("actions lost")
	}
}
