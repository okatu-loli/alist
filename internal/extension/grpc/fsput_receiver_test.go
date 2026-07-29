package grpc

import (
	"context"
	"io"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"google.golang.org/grpc"
)

// These tests exercise the REAL host/fsput.go receiver directly (the integration
// test in hostcap_authsign_integration_test.go goes through a subprocess; the
// hostcap fs_put_test.go bypasses fsput.go entirely by calling hostcap.FS().Put).
// The concern is HIGH-2: an ungranted or over-ceiling stream must be rejected
// before significant bytes are spooled to host temp disk.

// fakePutStream drives HostService_FSPutServer (a client-streaming server) from
// a scripted list of frames. It records how many CHUNK frames the receiver
// actually consumed (chunksConsumed) so a test can prove the spool never ran,
// and captures the final reply.
type fakePutStream struct {
	grpc.ServerStream // nil embed: unused ServerStream methods are never called
	ctx               context.Context
	frames            []*pb.FSPutClient
	i                 int
	chunksConsumed    int
	reply             *pb.FSPutReply
}

func (s *fakePutStream) Context() context.Context { return s.ctx }

func (s *fakePutStream) Recv() (*pb.FSPutClient, error) {
	if s.i >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	if f.GetChunk() != nil {
		s.chunksConsumed++
	}
	return f, nil
}

func (s *fakePutStream) SendAndClose(r *pb.FSPutReply) error {
	s.reply = r
	return nil
}

func initFrame(principal, dst, name string, size int64) *pb.FSPutClient {
	return &pb.FSPutClient{Msg: &pb.FSPutClient_Init{Init: &pb.FSPutInit{
		Principal: principal,
		DstDir:    pb.RefToProto(ext.ObjRef{Anchor: dst}),
		Info:      pb.UploadInfoToProto(ext.UploadInfo{Name: name, Size: size}),
	}}}
}

func chunkFrame(data []byte) *pb.FSPutClient {
	return &pb.FSPutClient{Msg: &pb.FSPutClient_Chunk{Chunk: &pb.Chunk{Data: data}}}
}

// An ungranted plugin's Put is rejected on the Init frame — before any chunk is
// consumed (chunksConsumed == 0), proving the pre-authorization gate stops the
// spool DoS.
func TestFSPutReceiverUngrantedRejectedBeforeSpool(t *testing.T) {
	mnt := "/rb-put-recv-nofs"
	mountHostcapLocal(t, mnt)

	// No CapFS grant at all: pre-auth must deny.
	h := hostcap.New(hostcap.Grant{PluginID: "recv.nofs"}, hostcap.Options{})
	srv := host.NewServer(h)

	st := &fakePutStream{
		ctx: context.Background(),
		frames: []*pb.FSPutClient{
			initFrame("", mnt, "hack.txt", 5),
			chunkFrame([]byte("hello")),
		},
	}
	if err := srv.FSPut(st); err != nil {
		t.Fatalf("FSPut returned transport error: %v", err)
	}
	if st.reply == nil || st.reply.Error == nil {
		t.Fatalf("ungranted Put: want error reply, got %+v", st.reply)
	}
	if st.chunksConsumed != 0 {
		t.Fatalf("ungranted Put must be rejected before spooling any chunk, consumed=%d", st.chunksConsumed)
	}
}

// A read-only grant is likewise refused on Init (write not authorized), again
// before any byte is spooled.
func TestFSPutReceiverReadOnlyRejectedBeforeSpool(t *testing.T) {
	mnt := "/rb-put-recv-ro"
	mountHostcapLocal(t, mnt)

	h := hostcap.New(hostcap.Grant{
		PluginID: "recv.ro",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt}}, // read-only
	}, hostcap.Options{})
	srv := host.NewServer(h)

	st := &fakePutStream{
		ctx: context.Background(),
		frames: []*pb.FSPutClient{
			initFrame("", mnt, "ro.txt", 5),
			chunkFrame([]byte("hello")),
		},
	}
	if err := srv.FSPut(st); err != nil {
		t.Fatalf("FSPut transport error: %v", err)
	}
	if st.reply == nil || st.reply.Error == nil {
		t.Fatalf("read-only Put: want error reply, got %+v", st.reply)
	}
	if st.chunksConsumed != 0 {
		t.Fatalf("read-only Put must be rejected before spooling, consumed=%d", st.chunksConsumed)
	}
}

// A write-granted stream that exceeds the byte ceiling is rejected mid-flight
// with an error reply (not an authorization pass to FS().Put).
func TestFSPutReceiverOverCeilingRejected(t *testing.T) {
	mnt := "/rb-put-recv-cap"
	mountHostcapLocal(t, mnt)

	h := hostcap.New(hostcap.Grant{
		PluginID: "recv.cap",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt, Write: true}},
	}, hostcap.Options{})
	srv := host.NewServer(h)

	// Lower the ceiling so a small stream trips it without allocating 10 GiB.
	restore := host.SetMaxHostPutBytesForTest(8)
	defer restore()

	st := &fakePutStream{
		ctx: context.Background(),
		frames: []*pb.FSPutClient{
			initFrame("", mnt, "big.txt", 100),
			chunkFrame([]byte("1234")),  // 4 bytes, under
			chunkFrame([]byte("56789")), // total 9 > 8 → reject
			chunkFrame([]byte("never")), // must never be consumed
		},
	}
	if err := srv.FSPut(st); err != nil {
		t.Fatalf("FSPut transport error: %v", err)
	}
	if st.reply == nil || st.reply.Error == nil {
		t.Fatalf("over-ceiling Put: want error reply, got %+v", st.reply)
	}
	if st.chunksConsumed != 2 {
		t.Fatalf("over-ceiling Put should stop after the frame that trips the ceiling (2 consumed), got %d", st.chunksConsumed)
	}
}

// Happy path still works through the real receiver: a write-granted, under-ceiling
// stream lands on disk and returns the object.
func TestFSPutReceiverHappyPath(t *testing.T) {
	mnt := "/rb-put-recv-ok"
	mountHostcapLocal(t, mnt)

	h := hostcap.New(hostcap.Grant{
		PluginID: "recv.ok",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt, Write: true}},
	}, hostcap.Options{})
	srv := host.NewServer(h)

	content := []byte(strings.Repeat("x", 1000))
	st := &fakePutStream{
		ctx: context.Background(),
		frames: []*pb.FSPutClient{
			initFrame("", mnt, "ok.txt", int64(len(content))),
			chunkFrame(content),
		},
	}
	if err := srv.FSPut(st); err != nil {
		t.Fatalf("FSPut transport error: %v", err)
	}
	if st.reply == nil || st.reply.Error != nil {
		t.Fatalf("happy path: want success reply, got %+v", st.reply)
	}
	if st.reply.Obj == nil || st.reply.Obj.Name != "ok.txt" {
		t.Fatalf("happy path: want obj ok.txt, got %+v", st.reply.Obj)
	}
}
