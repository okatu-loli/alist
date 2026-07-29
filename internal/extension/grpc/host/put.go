package host

import (
	"context"
	"io"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
)

// Put runs the host side of the pull-model upload bidi stream (9.0 §2). It sends
// the Init message, then serves the plugin's ReadRequest calls by reading from
// the host-held UploadStream via ReadAt, forwards Progress reports, and returns
// the final Obj the plugin reports on Done.
func (c *Client) Put(ctx context.Context, dstDir ext.ObjRef, up ext.UploadStream) (ext.Obj, error) {
	if !c.has(ext.CapPut) {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Put")
	}
	stream, err := c.svc.Put(ctx)
	if err != nil {
		return ext.Obj{}, err
	}
	if err := stream.Send(&pb.PutClient{Msg: &pb.PutClient_Init{Init: &pb.PutInit{
		DstDir: pb.RefToProto(dstDir),
		Info:   pb.UploadInfoToProto(up.Info()),
	}}}); err != nil {
		return ext.Obj{}, err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return ext.Obj{}, err
		}
		switch {
		case msg.GetReadRequest() != nil:
			rr := msg.GetReadRequest()
			buf := make([]byte, rr.Length)
			n, rerr := up.ReadAt(buf, rr.Offset)
			reply := &pb.ReadReply{Data: buf[:n]}
			if rerr == io.EOF {
				reply.Eof = true
			} else if rerr != nil {
				reply.Error = pb.ErrorToProto(rerr)
			}
			if serr := stream.Send(&pb.PutClient{Msg: &pb.PutClient_ReadReply{ReadReply: reply}}); serr != nil {
				return ext.Obj{}, serr
			}
		case msg.GetProgress() != nil:
			if c.onProgress != nil {
				c.onProgress(msg.GetProgress().Fraction)
			}
		case msg.GetDone() != nil:
			done := msg.GetDone()
			if e := pb.ErrorFromProto(done.Error); e != nil {
				return ext.Obj{}, e
			}
			return pb.ObjFromProto(done.Obj), nil
		}
	}
}
