package proto

import "time"

// This file holds wire-shape types shared by both ends that used to live
// inside agent-only files before the W3 proto-split (design analysis §6 W3,
// §7 R3): LogEntry (previously logring.go) and RapidUploadDetail plus the
// TaskStatus* constants (previously task.go). Moving them here changes
// NOTHING about their names, fields, or json tags — see wire_marshal_test.go
// for a byte-for-byte pin of that invariant, since LogEntry's tags in
// particular are the SP-G2 frontend contract.

// LogEntry is one captured agent log line. It is ALSO the wire shape the
// agent.log.tail channel method returns (see capproto.go's
// ChannelLogTailResp), so these json tags are the G2 frontend contract — keep
// them stable. internal/agent's LogRing (agent-side runtime buffer) stores
// and returns these; the server's observability endpoint decodes them
// straight off the wire.
type LogEntry struct {
	Seq   int64     `json:"seq"`
	TS    time.Time `json:"ts"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// Task status strings the agent reports and the server validates against,
// mirroring internal/model.AgentTask{Running,Done,Failed} literally (as
// string constants, not an import of internal/model: this protocol layer's
// dependency surface deliberately stays free of the DB-model layer — see
// server/handles.AgentTaskStatus for the server-side mirror of this same
// set).
const (
	TaskStatusRunning = "running"
	TaskStatusDone    = "done"
	TaskStatusFailed  = "failed"
)

// RapidUploadDetail is the done-status detail the agent reports when a copy
// task completed via hash-based rapid-upload ("秒传") instead of streaming
// bytes (design SP-H). It is a shared marker so the agent and the server
// agree on the exact string: the agent writes it on the done Status call,
// and the server's observability endpoint reads it back to surface a rapid
// flag to the UI.
const RapidUploadDetail = "rapid-upload"
