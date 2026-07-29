package extension

// ExecMode is how an extension's code is executed (Step 9.2). It is recorded per
// entry so the executor can later choose between running a built-in in-process
// and launching a signed plugin as a gRPC subprocess.
//
// 9.2 scope: this is an additive marker only. Built-in drivers are ExecInProcess
// and the op hot path is unchanged; wiring op dispatch to launch ExecGRPC
// subprocesses (the executor selection) lands with lifecycle in 9.3. The gRPC
// host client (internal/extension/grpc/host) is exercised directly by the 9.2
// parity tests.
type ExecMode int

const (
	// ExecInProcess runs the extension in the core process (built-in drivers via
	// the 9.1 bridge). Default.
	ExecInProcess ExecMode = iota
	// ExecGRPC runs the extension as a hashicorp/go-plugin gRPC subprocess.
	ExecGRPC
)

func (m ExecMode) String() string {
	switch m {
	case ExecInProcess:
		return "in-process"
	case ExecGRPC:
		return "grpc"
	default:
		return "unknown"
	}
}

// ExecMode reports the entry's execution mode.
func (e *Entry) ExecMode() ExecMode { return e.execMode }

// SetExecMode sets the entry's execution mode. Additive: no built-in entry is
// switched off in-process in 9.2.
func (e *Entry) SetExecMode(m ExecMode) { e.execMode = m }
