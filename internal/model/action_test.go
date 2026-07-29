// internal/model/action_test.go
package model

import "testing"

func TestActionsLegacyBitsRoundTrip(t *testing.T) {
	// 全 17 位往返
	all := int32(0x1FFFF)
	if got := ActionsFromLegacyBits(all).LegacyBits(); got != all {
		t.Errorf("round trip all bits: got %x want %x", got, all)
	}
	// 单个位:bit3 = write
	as := ActionsFromLegacyBits(1 << 3)
	if len(as) != 1 || as[0] != ActWrite {
		t.Errorf("bit3 should map to ActWrite, got %v", as)
	}
	// ActRead 没有 legacy 位,LegacyBits 忽略之
	if got := (Actions{ActRead, ActMove}).LegacyBits(); got != 1<<5 {
		t.Errorf("read has no legacy bit: got %x want %x", got, int32(1<<5))
	}
	// 零值
	if got := ActionsFromLegacyBits(0); len(got) != 0 {
		t.Errorf("zero bits -> empty, got %v", got)
	}
}

func TestActionsContains(t *testing.T) {
	as := Actions{ActRead, ActWrite}
	if !as.Contains(ActRead) || as.Contains(ActRemove) {
		t.Errorf("Contains broken: %v", as)
	}
}

func TestActionsValueScan(t *testing.T) {
	v, err := Actions{ActRead, ActWrite}.Value()
	if err != nil {
		t.Fatal(err)
	}
	var back Actions
	if err := back.Scan(v); err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0] != ActRead || back[1] != ActWrite {
		t.Errorf("scan back: %v", back)
	}
	var empty Actions
	if err := empty.Scan(nil); err != nil || empty != nil {
		t.Errorf("scan nil: %v %v", empty, err)
	}
}
