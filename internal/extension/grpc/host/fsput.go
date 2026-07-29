package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
)

// maxHostPutBytes is the hard ceiling on a single reverse-bridge upload's spool.
// The spool lands in $TMPDIR (often tmpfs/RAM), so without a cap a plugin — even
// one with NO fs grant, before pre-auth was added — could exhaust host temp disk.
// 10 GiB is a generous-but-bounded limit (larger than any realistic borrowed-FS
// upload) whose only job is to make the spool refuse an unbounded or hostile
// stream. ChunkSize bounds a single frame, never the stream total, so the
// running byte count is enforced during the Recv loop. It is a var (not a const)
// solely so a test can lower it to exercise the ceiling without streaming 10 GiB.
var maxHostPutBytes int64 = 10 << 30 // 10 GiB

// SetMaxHostPutBytesForTest lowers the spool ceiling and returns a restore func.
// It exists so the reverse-bridge receiver test can exercise the ceiling without
// streaming 10 GiB; production never calls it.
func SetMaxHostPutBytesForTest(n int64) (restore func()) {
	prev := maxHostPutBytes
	maxHostPutBytes = n
	return func() { maxHostPutBytes = prev }
}

// putPreAuthorizer is the optional hook a host-side ext.Host may implement to
// authorize a Put's destination BEFORE the receiver spools bytes. hostcap.Host
// satisfies it (host.go). Kept as a local structural interface so grpc/host does
// not import hostcap (which would be a cycle: hostcap is wired into this bridge,
// not the other way round).
type putPreAuthorizer interface {
	PreAuthorizePut(ctx context.Context, principal ext.Principal, dstDir ext.ObjRef) error
}

// FSPut is the host side of the REVERSE upload (a plugin subprocess borrowing
// host.FS().Put): unlike driver.proto's Put (a pull-model bidi stream where the
// host serves ReadAt because the DRIVER wants bytes it doesn't have), here the
// plugin already holds the bytes via its own ext.UploadStream, so it simply
// pushes an Init frame then Chunk frames (mirroring OpenRange's push shape).
// The host spools them to a temp file (to give the resulting UploadStream the
// random-access ReadAt shape FS().Put's contract expects) and forwards to
// s.host.FS().Put, which re-runs Authorize before anything is written.
func (s *HostServiceServer) FSPut(stream pb.HostService_FSPutServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	init := first.GetInit()
	if init == nil {
		return errors.New("host: first FSPut message must be Init")
	}

	// HIGH-2: authorize the destination WRITE on the Init frame, BEFORE spooling
	// any chunk, so an ungranted or unauthorized-destination Put is rejected with
	// ~0 bytes written to host temp disk. FS().Put still re-runs the full check;
	// this is the cheap early gate that closes the pre-auth spool DoS.
	if pa, ok := s.host.(putPreAuthorizer); ok {
		if err := pa.PreAuthorizePut(stream.Context(), ext.Principal(init.Principal), pb.RefFromProto(init.DstDir)); err != nil {
			return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(err)})
		}
	}

	up, err := newSpooledUpload(pb.UploadInfoFromProto(init.Info))
	if err != nil {
		return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(err)})
	}
	defer up.Close()

	var spooled int64
	for {
		msg, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
		chunk := msg.GetChunk()
		if chunk == nil {
			return errors.New("host: expected Chunk frame")
		}
		if e := pb.ErrorFromProto(chunk.Error); e != nil {
			return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(e)})
		}
		if len(chunk.Data) > 0 {
			// Enforce the hard ceiling on the running total: a stream that would
			// push the spool past maxHostPutBytes is refused mid-flight (the
			// defer up.Close() still removes the partial temp file).
			spooled += int64(len(chunk.Data))
			if spooled > maxHostPutBytes {
				err := fmt.Errorf("host: FSPut stream exceeds %d-byte ceiling: %w", maxHostPutBytes, io.ErrUnexpectedEOF)
				return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(err)})
			}
			if _, werr := up.Write(chunk.Data); werr != nil {
				return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(werr)})
			}
		}
	}

	obj, err := s.host.FS().Put(stream.Context(), ext.Principal(init.Principal), pb.RefFromProto(init.DstDir), up)
	if err != nil {
		return stream.SendAndClose(&pb.FSPutReply{Error: pb.ErrorToProto(err)})
	}
	return stream.SendAndClose(&pb.FSPutReply{Obj: pb.ObjToProto(obj)})
}

// spooledUpload lands a plugin-pushed byte stream in a temp file, giving it the
// ext.UploadStream (Info + ReadAt) shape FS().Put needs. It is write-then-read:
// every Write call must complete before ReadAt is used (FSPut only reads after
// the client stream is fully drained).
type spooledUpload struct {
	info ext.UploadInfo
	f    *os.File
}

func newSpooledUpload(info ext.UploadInfo) (*spooledUpload, error) {
	f, err := os.CreateTemp("", "alist-hostfsput-*")
	if err != nil {
		return nil, err
	}
	return &spooledUpload{info: info, f: f}, nil
}

func (u *spooledUpload) Info() ext.UploadInfo { return u.info }

func (u *spooledUpload) Write(p []byte) (int, error) { return u.f.Write(p) }

func (u *spooledUpload) ReadAt(p []byte, off int64) (int, error) { return u.f.ReadAt(p, off) }

func (u *spooledUpload) Close() error {
	name := u.f.Name()
	closeErr := u.f.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
