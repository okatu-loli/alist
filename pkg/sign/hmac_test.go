package sign

import "testing"

func TestHMACKidRoundTripAndGrace(t *testing.T) {
	keys := map[string][]byte{"k1": []byte("secret-one"), "k2": []byte("secret-two")}
	resolve := func(kid string) ([]byte, bool) { s, ok := keys[kid]; return s, ok }

	// signer on active k2
	signer := NewHMACSign(keys["k2"], "k2", resolve)
	tok := signer.Sign("/storage/1/path", 0)
	if err := signer.Verify("/storage/1/path", tok); err != nil {
		t.Fatalf("active verify: %v", err)
	}
	// a token minted under grace key k1 still verifies via the resolver
	graceSigner := NewHMACSign(keys["k1"], "k1", resolve)
	graceTok := graceSigner.Sign("/storage/1/path", 0)
	if err := signer.Verify("/storage/1/path", graceTok); err != nil {
		t.Fatalf("grace verify via active signer's resolver: %v", err)
	}
	// unknown kid rejected
	orphan := NewHMACSign([]byte("x"), "gone", func(string) ([]byte, bool) { return nil, false })
	if err := signer.Verify("/storage/1/path", orphan.Sign("/storage/1/path", 0)); err != ErrKeyUnknown {
		t.Fatalf("unknown kid must be ErrKeyUnknown, got %v", err)
	}
	// tampered kid (swap to k1's kid but keep k2 hmac) rejected
	tampered := tok[:len(tok)-2] + "k1"
	if err := signer.Verify("/storage/1/path", tampered); err == nil {
		t.Fatal("tampered kid must fail")
	}
	// wrong data rejected
	if err := signer.Verify("/storage/2/other", tok); err == nil {
		t.Fatal("wrong data must fail")
	}
}
