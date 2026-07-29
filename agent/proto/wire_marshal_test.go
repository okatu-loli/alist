package proto

import (
	"encoding/json"
	"testing"
	"time"
)

// TestLogEntryJSONTagsAreStable pins LogEntry's exact wire shape byte for
// byte: these json tags are the SP-G2 frontend contract (see wire.go's doc),
// and this file's move from logring.go into the proto package (design
// analysis §7 R3) must not silently rename a field. A future accidental
// rename shows up here as a diff against this fixed string, not as a
// mysteriously blank column in the admin UI.
func TestLogEntryJSONTagsAreStable(t *testing.T) {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	e := LogEntry{Seq: 7, TS: ts, Level: "info", Msg: "hello"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"seq":7,"ts":"2026-07-26T12:00:00Z","level":"info","msg":"hello"}`
	if string(b) != want {
		t.Fatalf("LogEntry JSON shape changed:\n got:  %s\n want: %s", b, want)
	}
}

// TestTaskStatusAndRapidUploadDetailLiteralsAreStable pins the exact string
// literals the agent and server agree on out of band (they are compared as
// plain strings over the wire, not marshaled struct fields, but a silent
// rename here would break status-transition and rapid-upload-flag agreement
// just as surely as a json tag rename would).
func TestTaskStatusAndRapidUploadDetailLiteralsAreStable(t *testing.T) {
	cases := map[string]string{
		"TaskStatusRunning": TaskStatusRunning,
		"TaskStatusDone":    TaskStatusDone,
		"TaskStatusFailed":  TaskStatusFailed,
		"RapidUploadDetail": RapidUploadDetail,
	}
	want := map[string]string{
		"TaskStatusRunning": "running",
		"TaskStatusDone":    "done",
		"TaskStatusFailed":  "failed",
		"RapidUploadDetail": "rapid-upload",
	}
	for name, got := range cases {
		if got != want[name] {
			t.Fatalf("%s = %q, want %q", name, got, want[name])
		}
	}
}

// TestChannelLogTailRespJSONTagsAreStable pins the wrapper response's own
// json tags (entries/next) alongside the embedded LogEntry shape, since
// capproto.go's ChannelLogTailResp is what actually crosses the wire.
func TestChannelLogTailRespJSONTagsAreStable(t *testing.T) {
	ts := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	resp := ChannelLogTailResp{
		Entries: []LogEntry{{Seq: 1, TS: ts, Level: "warning", Msg: "m"}},
		Next:    1,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"entries":[{"seq":1,"ts":"2026-07-26T12:00:00Z","level":"warning","msg":"m"}],"next":1}`
	if string(b) != want {
		t.Fatalf("ChannelLogTailResp JSON shape changed:\n got:  %s\n want: %s", b, want)
	}
}
