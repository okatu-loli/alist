package channel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pub, priv
}

// wsHandshake runs the agent leg inside a loopback WebSocket server and the
// server leg in the test body, returning both legs' errors. Mirrors the real
// topology: the AGENT listens, the SERVER dials in.
func wsHandshake(t *testing.T, agentLeg, serverLeg func(conn *websocket.Conn) error) (srvErr, agtErr error) {
	t.Helper()
	agentErrCh := make(chan error, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			agentErrCh <- err
			return
		}
		defer conn.Close()
		agentErrCh <- agentLeg(conn)
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	srvErr = serverLeg(conn)
	select {
	case agtErr = <-agentErrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("agent leg did not return")
	}
	return srvErr, agtErr
}

const testAgentID = "agent-under-test"

func TestHandshakeHappyPath(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()

	srvErr, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
			return err
		},
	)
	if srvErr != nil {
		t.Fatalf("server leg: %v", srvErr)
	}
	if agtErr != nil {
		t.Fatalf("agent leg: %v", agtErr)
	}
}

// Both legs derive the SAME 32-byte session key from the authenticated
// ephemeral ECDH — the property the whole AEAD layer rests on.
func TestHandshakeDerivesSharedSessionKey(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()

	var serverKey, agentKey []byte
	srvErr, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			k, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, now, nil)
			agentKey = k
			return err
		},
		func(conn *websocket.Conn) error {
			k, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
			serverKey = k
			return err
		},
	)
	if srvErr != nil || agtErr != nil {
		t.Fatalf("handshake failed: srv=%v agt=%v", srvErr, agtErr)
	}
	if len(serverKey) != aeadKeyBytes {
		t.Fatalf("server key must be %d bytes, got %d", aeadKeyBytes, len(serverKey))
	}
	if !bytes.Equal(serverKey, agentKey) {
		t.Fatalf("both sides must derive the same session key:\n server=%x\n agent =%x", serverKey, agentKey)
	}
}

// Forward-secrecy independence: two separate handshakes (fresh ephemerals each)
// derive INDEPENDENT session keys — recovering one tells you nothing about the
// other, and the long-term ed25519 keys never enter the key derivation.
func TestDistinctHandshakesDeriveDistinctKeys(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()

	run := func() []byte {
		var serverKey []byte
		srvErr, agtErr := wsHandshake(t,
			func(conn *websocket.Conn) error {
				_, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, now, nil)
				return err
			},
			func(conn *websocket.Conn) error {
				k, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
				serverKey = k
				return err
			},
		)
		if srvErr != nil || agtErr != nil {
			t.Fatalf("handshake failed: srv=%v agt=%v", srvErr, agtErr)
		}
		return serverKey
	}

	k1, k2 := run(), run()
	if bytes.Equal(k1, k2) {
		t.Fatal("two handshakes must derive different session keys (forward secrecy: fresh ephemerals)")
	}
}

// A MITM that substitutes eph_s (leaving sig_s signing the ORIGINAL ephemeral)
// is rejected: the agent recomputes the signed message over the WIRE eph_s, so
// the signature no longer verifies — a MITM cannot swap an ephemeral.
func TestTamperedServerEphemeralRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()
	_ = agentPub

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			// Sign over the REAL ephemeral, but put a DIFFERENT (also valid)
			// ephemeral on the wire — exactly a MITM substitution.
			realEph, err := generateEphemeral()
			if err != nil {
				return err
			}
			otherEph, err := generateEphemeral()
			if err != nil {
				return err
			}
			nonceS, err := randNonce()
			if err != nil {
				return err
			}
			ts := now.Unix()
			sig := ed25519.Sign(serverPriv, serverHelloSigningMessage(testAgentID, nonceS, ephemeralPublicB64(realEph), ts))
			return writeHandshakeJSON(conn, ServerHello{
				Type:    serverHelloType,
				AgentID: testAgentID,
				NonceS:  nonceS,
				EphS:    ephemeralPublicB64(otherEph), // substituted ephemeral
				Ts:      ts,
				Sig:     base64Std(sig),
			})
		},
	)
	if agtErr == nil {
		t.Fatal("agent must reject a server hello whose eph_s does not match sig_s")
	}
	if !strings.Contains(agtErr.Error(), "signature failed verification") {
		t.Fatalf("expected signature-failure error, got: %v", agtErr)
	}
}

// Server signature signed by a DIFFERENT key than the agent pinned → the agent
// rejects and the channel never lives (pinning enforced).
func TestServerSignedByWrongKeyRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	_, serverPriv := genKey(t) // server actually signs with this
	pinnedPub, _ := genKey(t)  // agent pinned a DIFFERENT server key
	_ = agentPub
	now := time.Now()

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, pinnedPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
			return err
		},
	)
	if agtErr == nil {
		t.Fatal("agent must reject a server hello not signed by the pinned key")
	}
	if !strings.Contains(agtErr.Error(), "pinned") {
		t.Fatalf("expected pinning-failure error, got: %v", agtErr)
	}
}

// Agent has NO pinned server key → it refuses the reverse connection outright
// (fail closed), before reading anything.
func TestAgentNoPinnedKeyFailsClosed(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	_, serverPriv := genKey(t)
	now := time.Now()

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, nil /* no pinned key */, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
			return err
		},
	)
	if agtErr == nil {
		t.Fatal("agent with no pinned server key must fail closed")
	}
	if !strings.Contains(agtErr.Error(), "fail closed") && !strings.Contains(agtErr.Error(), "no pinned") {
		t.Fatalf("expected fail-closed error, got: %v", agtErr)
	}
}

// Agent signature signed by a different key than the server expects → server
// rejects.
func TestAgentSignedByWrongKeyRejected(t *testing.T) {
	agentPubExpected, _ := genKey(t) // server verifies against this
	_, wrongAgentPriv := genKey(t)   // agent actually signs with this
	serverPub, serverPriv := genKey(t)
	now := time.Now()

	srvErr, _ := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, wrongAgentPriv, testAgentID, serverPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPubExpected, testAgentID, now)
			return err
		},
	)
	if srvErr == nil {
		t.Fatal("server must reject an agent hello not signed by the stored key")
	}
	if !strings.Contains(srvErr.Error(), "signature failed verification") {
		t.Fatalf("expected agent-sig-failure error, got: %v", srvErr)
	}
}

// A ServerHello whose ts is stale (a captured/replayed hello) is rejected by
// the agent even though the signature is valid.
func TestStaleServerHelloRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	stale := time.Now().Add(-1 * time.Hour) // server signs a stale ts
	agentNow := time.Now()

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, agentNow, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, stale)
			return err
		},
	)
	if agtErr == nil {
		t.Fatal("agent must reject a stale server hello")
	}
	if !strings.Contains(agtErr.Error(), "stale") {
		t.Fatalf("expected stale error, got: %v", agtErr)
	}
}

// A stale AgentHello ts is likewise rejected by the server. The agent leg is
// hand-crafted so it accepts the (fresh) server hello but replies with a
// correctly-signed hello bearing a stale timestamp — exactly a captured-and-
// replayed AgentHello.
func TestStaleAgentHelloRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()
	staleTs := now.Add(-1 * time.Hour).Unix()

	srvErr, _ := wsHandshake(t,
		func(conn *websocket.Conn) error {
			var sh ServerHello
			if err := readHandshakeJSON(conn, &sh); err != nil {
				return err
			}
			_ = serverPub
			eph, err := generateEphemeral()
			if err != nil {
				return err
			}
			ephA := ephemeralPublicB64(eph)
			nonceA := "agent-nonce"
			sig := ed25519.Sign(agentPriv, agentHelloSigningMessage(testAgentID, sh.NonceS, nonceA, ephA, staleTs))
			return writeHandshakeJSON(conn, AgentHello{
				Type:   agentHelloType,
				NonceA: nonceA,
				EphA:   ephA,
				Ts:     staleTs,
				Sig:    base64Std(sig),
			})
		},
		func(conn *websocket.Conn) error {
			_, err := RunServerHandshake(conn, serverPriv, agentPub, testAgentID, now)
			return err
		},
	)
	if srvErr == nil {
		t.Fatal("server must reject a stale agent hello")
	}
	if !strings.Contains(srvErr.Error(), "stale") {
		t.Fatalf("expected stale error, got: %v", srvErr)
	}
}

// A ServerHello addressed to a different agent_id is rejected.
func TestServerHelloWrongAgentIDRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, serverPriv := genKey(t)
	now := time.Now()

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, "this-agent", serverPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			// server addresses a DIFFERENT agent
			_, err := RunServerHandshake(conn, serverPriv, agentPub, "other-agent", now)
			return err
		},
	)
	if agtErr == nil {
		t.Fatal("agent must reject a hello addressed to a different agent")
	}
}

// The agent's signature BINDS the server's nonce, so it is fresh to THIS
// handshake: a signature produced for one nonceS cannot verify under another.
func TestAgentSigBindsServerNonce(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	nonceS1 := "nonce-from-handshake-1"
	nonceS2 := "nonce-from-handshake-2"
	nonceA := "agent-nonce"
	ephA := "agent-eph"
	ts := time.Now().Unix()

	sig := ed25519.Sign(agentPriv, agentHelloSigningMessage(testAgentID, nonceS1, nonceA, ephA, ts))
	// Same sig, verified against a DIFFERENT server nonce → must fail (a
	// replayed/precomputed agent sig cannot be lifted into another handshake).
	if ed25519.Verify(agentPub, agentHelloSigningMessage(testAgentID, nonceS2, nonceA, ephA, ts), sig) {
		t.Fatal("agent sig must not verify under a different server nonce")
	}
	// Sanity: it verifies under the correct nonce.
	if !ed25519.Verify(agentPub, agentHelloSigningMessage(testAgentID, nonceS1, nonceA, ephA, ts), sig) {
		t.Fatal("agent sig should verify under its own nonce")
	}
}

// Domain separation: a signature over the server-leg message cannot satisfy the
// agent-leg verification and vice versa, even with the same key — the distinct
// prefixes make the signed bytes disjoint.
func TestDomainSeparationBetweenLegs(t *testing.T) {
	pub, priv := genKey(t)
	nonceS := "ns"
	nonceA := "na"
	ephS := "es"
	ephA := "ea"
	ts := time.Now().Unix()

	serverMsg := serverHelloSigningMessage(testAgentID, nonceS, ephS, ts)
	agentMsg := agentHelloSigningMessage(testAgentID, nonceS, nonceA, ephA, ts)

	sigServer := ed25519.Sign(priv, serverMsg)
	sigAgent := ed25519.Sign(priv, agentMsg)

	if ed25519.Verify(pub, agentMsg, sigServer) {
		t.Fatal("a server-leg signature must not verify as an agent-leg signature")
	}
	if ed25519.Verify(pub, serverMsg, sigAgent) {
		t.Fatal("an agent-leg signature must not verify as a server-leg signature")
	}
}

// Domain separation from the heartbeat: the handshake messages carry prefixes
// the heartbeat message never has, so a captured heartbeat signature can't be
// reused in the handshake.
func TestDomainSeparationFromHeartbeat(t *testing.T) {
	serverMsg := string(serverHelloSigningMessage(testAgentID, "ns", "es", 1))
	agentMsg := string(agentHelloSigningMessage(testAgentID, "ns", "na", "ea", 1))
	if !strings.HasPrefix(serverMsg, serverHelloPrefix+"|") {
		t.Fatal("server hello message missing its domain prefix")
	}
	if !strings.HasPrefix(agentMsg, agentHelloPrefix+"|") {
		t.Fatal("agent hello message missing its domain prefix")
	}
	// The heartbeat message (agent.HeartbeatMessage) is "agentID|ts|nonce" —
	// it starts with the agent id, never with these prefixes.
	if strings.HasPrefix(testAgentID, serverHelloPrefix) || strings.HasPrefix(testAgentID, agentHelloPrefix) {
		t.Fatal("test agent id accidentally collides with a handshake prefix")
	}
}

// Oversized handshake message is rejected by the bounded reader.
func TestOversizedHandshakeRejected(t *testing.T) {
	agentPub, agentPriv := genKey(t)
	serverPub, _ := genKey(t)
	now := time.Now()

	_, agtErr := wsHandshake(t,
		func(conn *websocket.Conn) error {
			_, err := RunAgentHandshake(conn, agentPriv, testAgentID, serverPub, now, nil)
			return err
		},
		func(conn *websocket.Conn) error {
			// Instead of a valid hello, blast a message far over the cap.
			_ = conn.WriteMessage(websocket.TextMessage, make([]byte, maxHandshakeBytes+1))
			return nil
		},
	)
	_ = agentPub
	if agtErr == nil {
		t.Fatal("agent must reject an oversized handshake message")
	}
}
