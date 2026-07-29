package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// This file is the AGENT PAIRING WEB (design subproject F, wave F2): the small,
// self-contained, LOCAL control surface an operator uses to arm token-
// authenticated reverse pairing. `alist agent serve --pair --pair-web <addr>`
// serves it; the operator (reaching it over an SSH tunnel to the default
// 127.0.0.1 bind) pastes the one-time token T the server generated, clicks
// "arm", and watches the status as the server dials in and the token mutual-
// verify runs. Pasting T + arming IS the operator's affirmative action — it
// REPLACES the retired terminal y/n confirm (design F, F2 req #3): a passing
// token verify pins the server key + brings the data channel live with NO
// separate human step.
//
// 🔒 THIS PAGE HANDLES A SECRET (T). It is treated as a secret-handling surface:
//   - CONFIDENTIALITY OF THE BIND (design F Critical #3): the token is a SECRET
//     that grants a full MITM if it leaks, so the default bind is 127.0.0.1 and a
//     NON-loopback plain-HTTP bind is REFUSED unless the operator explicitly
//     acknowledges it (--pair-web-allow-public) — see ResolvePairWebBind. A public
//     plain-HTTP bind is never SILENTLY allowed.
//   - RESTRICTIVE HEADERS: every response sets no-store / no-referrer / nosniff /
//     DENY-framing, and the page ships a strict CSP with NO external origins, so
//     the pasted T is never cached, referred, framed, or exfiltrated to a third
//     party.
//   - NO EXTERNAL ASSETS / NO FRAMEWORK: the page is one embedded HTML string with
//     inline CSS + JS; it never fetches anything off-box.
//
// The web owns the agent-side token LIFECYCLE (design F Important #5): the armed
// T has a TTL (reversePairWebTokenTTL, mirroring the server's ~10min); it is
// single-use = burned on success OR on TTL expiry, but NOT on a single failed/
// dropped attempt (a dropped connection must let the operator retry within the
// TTL). On expiry the acceptor is auto-disarmed and T cleared from memory
// (PairingAcceptor.Disarm zeroes it). It never leaves a pasted T armed forever.

// reversePairWebTokenTTL is how long a pasted token stays armed before the web
// auto-disarms the acceptor and clears T (design F Important #5, mirrors the
// server-side ≤10min one-time token TTL).
const reversePairWebTokenTTL = 10 * time.Minute

// reversePairWebCSP is the page's Content-Security-Policy: no external origins at
// all. Inline script/style are needed (the page is a single self-contained file)
// so 'unsafe-inline' is granted ONLY for script-src/style-src; connect-src 'self'
// lets the page poll its own /status + POST /arm; everything else is denied.
const reversePairWebCSP = "default-src 'none'; " +
	"script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

// ReversePairPhase is the coarse state the web status reports.
type ReversePairPhase string

const (
	// PhaseIdle: no token armed yet (or never armed).
	PhaseIdle ReversePairPhase = "idle"
	// PhaseWaiting: a token is armed and the acceptor is waiting for the server to
	// dial in.
	PhaseWaiting ReversePairPhase = "waiting"
	// PhaseVerifying: a dial-in upgraded and the token handshake is running.
	PhaseVerifying ReversePairPhase = "verifying"
	// PhaseSuccess: the token verified, the server key is pinned, the data channel
	// is live. Terminal.
	PhaseSuccess ReversePairPhase = "success"
	// PhaseFailed: the last attempt failed (wrong/short token, tampered transcript,
	// dropped connection). Still ARMED within the TTL for a retry.
	PhaseFailed ReversePairPhase = "failed"
	// PhaseExpired: the token TTL elapsed; the acceptor was auto-disarmed and T
	// cleared. The operator must generate a fresh token on the server. Terminal
	// until re-armed.
	PhaseExpired ReversePairPhase = "expired"
)

// PairingWeb is the local pairing control surface. Build it with NewPairingWeb
// around an already-constructed PairingAcceptor; mount its ServeHTTP on a LOCAL
// (default 127.0.0.1) http.Server, separate from the public --listen mux the
// acceptor's /agentpair endpoint lives on. It arms the acceptor when T is pasted,
// runs the token TTL, and reports status the page polls.
type PairingWeb struct {
	acceptor *PairingAcceptor
	ttl      time.Duration
	now      func() time.Time

	// OnPaired, if set, is invoked AFTER the token verified and the key was pinned
	// (chained after the web records success), so the cmd layer can bring the R2
	// data channel live. Mirrors PairingAcceptor.OnPaired's signature.
	OnPaired func(cfg ClientConfig, serverPub []byte, fingerprint string)

	mu          sync.Mutex
	phase       ReversePairPhase
	expiresAt   time.Time
	timer       *time.Timer
	agentID     string
	fingerprint string
	reason      string
}

// NewPairingWeb wraps acc as the token source + consent surface. It starts the
// acceptor DISARMED (only a pasted+armed T opens it) and wires the acceptor's
// consent gate to auto-true — paste+arm is the operator's affirmative action, so
// a passing token verify pins + goes live with no separate confirm.
func NewPairingWeb(acc *PairingAcceptor) *PairingWeb {
	pw := &PairingWeb{
		acceptor: acc,
		ttl:      reversePairWebTokenTTL,
		now:      time.Now,
		phase:    PhaseIdle,
	}
	// Start disarmed: nothing pairs until the operator pastes a token.
	acc.Disarm()
	// Paste T + arm IS the consent; a passing tag verify pins + goes live.
	acc.Confirm = func(string) bool { return true }
	acc.OnAttemptStart = pw.onAttemptStart
	acc.OnAttemptError = pw.onAttemptError
	acc.OnPaired = pw.onPaired
	acc.OnError = pw.onPersistError
	return pw
}

// Arm canonicalizes+validates a pasted token and arms the acceptor with it. A
// token that does not canonicalize to the fixed 32 raw bytes is REJECTED here —
// the acceptor is NOT armed (design F: wrong/short T rejected at the web layer,
// no arm). On success it (re)starts the TTL timer and moves to PhaseWaiting.
func (pw *PairingWeb) Arm(pasted string) error {
	raw, err := proto.CanonicalizeReversePairToken(pasted)
	if err != nil {
		return err
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.acceptor.Arm(raw)
	// The stored copy is the acceptor's; zero our transient decode.
	for i := range raw {
		raw[i] = 0
	}
	pw.phase = PhaseWaiting
	pw.reason = ""
	pw.agentID = ""
	pw.fingerprint = ""
	pw.expiresAt = pw.now().Add(pw.ttl)
	if pw.timer != nil {
		pw.timer.Stop()
	}
	pw.timer = time.AfterFunc(pw.ttl, pw.expire)
	return nil
}

// expire is the TTL callback: it auto-disarms the acceptor (which clears T from
// memory) and moves to PhaseExpired — UNLESS a success already landed in the
// race window, which it must not clobber.
func (pw *PairingWeb) expire() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.phase == PhaseSuccess {
		return
	}
	pw.acceptor.Disarm() // zeroes + clears the armed token
	pw.phase = PhaseExpired
	pw.reason = "配对令牌已过期(TTL 到),已自动撤防并清除令牌;请在服务器重新生成令牌后再试"
}

func (pw *PairingWeb) onAttemptStart() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.phase == PhaseWaiting || pw.phase == PhaseFailed {
		pw.phase = PhaseVerifying
	}
}

func (pw *PairingWeb) onAttemptError(err error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	// A success or an expiry already ran — don't downgrade a terminal state.
	if pw.phase == PhaseSuccess || pw.phase == PhaseExpired {
		return
	}
	// A dropped/failed attempt leaves the acceptor ARMED within the TTL; the
	// operator can retry WITHOUT re-pasting (single-use burns on success-or-TTL,
	// not on one failed attempt).
	pw.phase = PhaseFailed
	pw.reason = err.Error()
}

func (pw *PairingWeb) onPaired(cfg ClientConfig, serverPub []byte, fingerprint string) {
	pw.mu.Lock()
	pw.phase = PhaseSuccess
	pw.agentID = cfg.AgentID
	pw.fingerprint = fingerprint
	pw.reason = ""
	if pw.timer != nil {
		pw.timer.Stop()
	}
	cb := pw.OnPaired
	pw.mu.Unlock()
	if cb != nil {
		cb(cfg, serverPub, fingerprint)
	}
}

func (pw *PairingWeb) onPersistError(err error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.phase == PhaseSuccess || pw.phase == PhaseExpired {
		return
	}
	pw.phase = PhaseFailed
	pw.reason = "配对已验证但本地持久化失败: " + err.Error()
}

// reversePairStatus is the JSON the page polls from GET /status.
type reversePairStatus struct {
	Phase       ReversePairPhase `json:"phase"`
	AgentID     string           `json:"agent_id,omitempty"`
	Fingerprint string           `json:"fingerprint,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	// ExpiresInSeconds is the whole seconds left on the armed token's TTL, for the
	// live states only (0 otherwise).
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

// Status returns the current status snapshot (exported for tests + the cmd
// layer).
func (pw *PairingWeb) Status() reversePairStatus {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	st := reversePairStatus{
		Phase:       pw.phase,
		AgentID:     pw.agentID,
		Fingerprint: pw.fingerprint,
		Reason:      pw.reason,
	}
	switch pw.phase {
	case PhaseWaiting, PhaseVerifying, PhaseFailed:
		if !pw.expiresAt.IsZero() {
			rem := pw.expiresAt.Sub(pw.now())
			if rem < 0 {
				rem = 0
			}
			st.ExpiresInSeconds = int64(rem / time.Second)
		}
	}
	return st
}

// ServeHTTP routes the three endpoints: GET / (page), POST /arm (arm with a
// pasted T), GET /status (poll). Every response carries the secret-handling
// headers.
func (pw *PairingWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecretHeaders(w)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", reversePairWebCSP)
		_, _ = w.Write([]byte(reversePairWebHTML))
	case r.Method == http.MethodPost && r.URL.Path == "/arm":
		pw.handleArm(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(pw.Status())
	default:
		http.NotFound(w, r)
	}
}

// handleArm reads the pasted token from a small JSON body, canonicalizes+
// validates it via Arm, and returns the fresh status. A malformed/short token is
// a 400 and the acceptor is NOT armed.
func (pw *PairingWeb) handleArm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeArmError(w, "无法解析请求体")
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeArmError(w, "请粘贴服务器生成的配对令牌")
		return
	}
	if err := pw.Arm(body.Token); err != nil {
		// Wrong/short token — rejected at the web layer, NO arm.
		writeArmError(w, "配对令牌无效: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(pw.Status())
}

func writeArmError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// setSecretHeaders applies the secret-handling response headers to EVERY response
// this surface emits (the page, /arm, /status): the pasted T must never be
// cached, referred to a third party, sniffed, or framed.
func setSecretHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}

// ResolvePairWebBind normalizes the --pair-web bind address and ENFORCES the
// confidentiality rule (design F Critical #3). Because this surface carries the
// SECRET token T, a NON-loopback PLAIN-HTTP bind is never SILENTLY allowed:
//
//   - A bare port ("5344") or an empty host (":5344") defaults the host to
//     127.0.0.1, so the common case is loopback-only and the operator reaches it
//     over an SSH tunnel (ssh -L LOCAL:127.0.0.1:5344 ...).
//   - A loopback host (127.0.0.0/8, ::1, "localhost") is allowed with no warning.
//   - Any other host — including the "bind all interfaces" 0.0.0.0 / :: — is
//     REFUSED unless the operator explicitly passes --pair-web-allow-public
//     (allowPublic), in which case it is allowed but returns a prominent warning
//     that T transits in CLEARTEXT unless fronted by TLS.
//
// It returns the resolved host:port, an optional warning to log, and an error
// that (when non-nil) must ABORT startup.
func ResolvePairWebBind(addr string, allowPublic bool) (resolved string, warn string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", errPairWeb("--pair-web requires an address or port, e.g. --pair-web 127.0.0.1:5344 or --pair-web 5344")
	}
	// A bare port (no colon) means "default host + this port".
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return "", "", errPairWeb("--pair-web address is malformed (want host:port or a bare port): " + splitErr.Error())
	}
	if port == "" {
		return "", "", errPairWeb("--pair-web address must include a port")
	}
	if _, perr := strconv.Atoi(port); perr != nil {
		return "", "", errPairWeb("--pair-web port must be numeric, got " + strconv.Quote(port))
	}
	if host == "" {
		// Default to loopback: the safe common case reached via SSH tunnel.
		host = "127.0.0.1"
	}

	if isLoopbackHost(host) {
		return net.JoinHostPort(host, port), "", nil
	}

	// Non-loopback (a routable IP, a hostname, or the 0.0.0.0/:: "all interfaces"
	// bind). This exposes T over plain HTTP.
	if !allowPublic {
		return "", "", errPairWeb("--pair-web " + addr + " binds a NON-loopback address, which would expose the one-time pairing token over PLAIN HTTP — refusing. " +
			"Keep the default 127.0.0.1 bind and reach it over an SSH tunnel (ssh -L 5344:127.0.0.1:5344 your-agent-host), " +
			"or, if you really must bind publicly, pass --pair-web-allow-public AND front it with TLS. A public plain-HTTP bind is not allowed silently.")
	}
	return net.JoinHostPort(host, port),
		"⚠ --pair-web is bound to a NON-loopback address (" + net.JoinHostPort(host, port) + ") over PLAIN HTTP. " +
			"The one-time pairing token you paste will transit in CLEARTEXT unless you have fronted this port with TLS. " +
			"Prefer the default 127.0.0.1 bind reached via `ssh -L`.",
		nil
}

// isLoopbackHost reports whether host is a loopback destination: "localhost", or
// an IP in 127.0.0.0/8 or ::1. The unspecified addresses 0.0.0.0 / :: are NOT
// loopback — they bind every interface — so they fall through to the public
// guard.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// errPairWeb is a thin helper so every bind error reads consistently.
func errPairWeb(msg string) error { return &pairWebError{msg} }

type pairWebError struct{ msg string }

func (e *pairWebError) Error() string { return e.msg }

// reversePairWebHTML is the minimal, self-contained pairing page (design F, F2:
// functional not polished — F4 Figma-designs the real UI). One text field for T,
// an "arm" button, and a status area that polls /status. No external assets, no
// framework; inline CSS + JS only. Copy is minimal and factual — no marketing or
// banner text. The pasted T is cleared from the input the moment it is armed.
const reversePairWebHTML = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Agent 反向配对</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 system-ui, sans-serif; max-width: 640px; margin: 2rem auto; padding: 0 1rem; }
  h1 { font-size: 1.2rem; }
  label { display: block; margin: 1rem 0 .3rem; font-weight: 600; }
  textarea { width: 100%; box-sizing: border-box; font-family: ui-monospace, monospace; font-size: .95rem; padding: .5rem; }
  button { margin-top: .8rem; padding: .5rem 1.1rem; font-size: 1rem; cursor: pointer; }
  button:disabled { cursor: default; opacity: .6; }
  #status { margin-top: 1.4rem; padding: .8rem 1rem; border: 1px solid currentColor; border-radius: 6px; }
  .k { opacity: .7; }
  code { font-family: ui-monospace, monospace; word-break: break-all; }
  .ok { color: #1a7f37; }
  .err { color: #b3261e; }
</style>
</head>
<body>
<h1>Agent 反向配对</h1>
<p>粘贴服务器生成的一次性令牌,点击「布防」。服务器随后拨入并与本机用该令牌互相校验;校验通过即钉定服务器密钥并激活数据通道。</p>
<label for="tok">配对令牌 (T)</label>
<textarea id="tok" rows="3" autocomplete="off" spellcheck="false" autocapitalize="off"></textarea>
<button id="arm">布防配对</button>
<div id="status" aria-live="polite">状态:未布防</div>
<script>
(function () {
  var tok = document.getElementById('tok');
  var arm = document.getElementById('arm');
  var box = document.getElementById('status');
  var polling = false;

  // Build status via createElement + textContent ONLY. Dynamic values (reason,
  // agent_id, fingerprint) MUST NOT be interpolated into markup — s.reason can
  // carry peer-influenced bytes, so injecting it as HTML would be a DOM-XSS on
  // this secret-handling page. Keep this file free of the HTML-assignment sink.
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  function kv(labelText, valueText, mono) {
    var wrap = el('div');
    wrap.appendChild(el('span', 'k', labelText + ' '));
    wrap.appendChild(el(mono ? 'code' : 'span', null, valueText));
    return wrap;
  }
  function render(s) {
    var m = {
      idle: '未布防',
      waiting: '已布防,等待服务器拨入…',
      verifying: '校验中…',
      success: '成功 ✓',
      failed: '上次尝试失败(仍布防,可在有效期内重试)',
      expired: '令牌已过期,已自动撤防'
    };
    var phaseText = m[s.phase] || s.phase;
    box.textContent = '';
    if (s.phase === 'success') {
      box.appendChild(el('span', 'ok', '状态:成功 ✓'));
      if (s.agent_id) box.appendChild(kv('agent_id:', s.agent_id, true));
      if (s.fingerprint) box.appendChild(kv('服务器密钥指纹:', s.fingerprint, true));
    } else if (s.phase === 'failed' || s.phase === 'expired') {
      box.appendChild(el('span', 'err', '状态:' + phaseText));
      if (s.reason) box.appendChild(kv('原因:', s.reason, false));
    } else {
      box.appendChild(el('span', null, '状态:' + phaseText));
      if ((s.phase === 'waiting' || s.phase === 'verifying') && s.expires_in_seconds) {
        box.appendChild(kv('令牌剩余有效期:', s.expires_in_seconds + ' 秒', false));
      }
    }
  }

  function poll() {
    fetch('/status', { cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (s) {
        render(s);
        if (s.phase === 'success' || s.phase === 'expired' || s.phase === 'idle') { polling = false; return; }
        setTimeout(poll, 1500);
      })
      .catch(function () { setTimeout(poll, 2500); });
  }

  arm.addEventListener('click', function () {
    var t = tok.value;
    if (!t.trim()) { box.textContent = '状态:请先粘贴令牌'; return; }
    arm.disabled = true;
    fetch('/arm', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      cache: 'no-store',
      body: JSON.stringify({ token: t })
    }).then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
      .then(function (res) {
        arm.disabled = false;
        if (!res.ok) { box.textContent = ''; box.appendChild(el('span', 'err', '状态:' + (res.j.error || '布防失败'))); return; }
        // Clear the secret from the field once it is armed.
        tok.value = '';
        render(res.j);
        if (!polling) { polling = true; poll(); }
      }).catch(function () { arm.disabled = false; box.textContent = '状态:请求失败'; });
  });
})();
</script>
</body>
</html>
`
