package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/AlistGo/alist-agent/channel"
	"github.com/AlistGo/alist-agent/proto"
)

// channelCapClient is the reverse-mode CapClient (design R3): it drives the
// four capability operations as frames over a live, mutual-authed,
// AEAD-encrypted reverse *channel.Channel instead of over HTTP. RunCopyTaskWith
// runs identically over it and over httpCapClient — the only thing that differs
// is the wire. Server-side, every frame it sends is re-authorized through the
// SAME shared core the HTTP endpoints use (server/handles/agent_task_core.go):
// the channel is transport, not authorization.
type channelCapClient struct {
	ch *channel.Channel
}

// NewChannelCapClient builds a reverse-mode CapClient over ch. The caller owns
// the channel's lifecycle; this client only sends on it.
func NewChannelCapClient(ch *channel.Channel) CapClient { return &channelCapClient{ch: ch} }

// streamChunkIDLen sizes the random per-upload stream id — not a secret (the
// cap token is), just a correlation handle unique among concurrent uploads on
// one channel.
const streamChunkIDLen = 16

func (c *channelCapClient) SrcLink(ctx context.Context, taskID, capToken string) (SrcLink, error) {
	payload, err := json.Marshal(proto.ChannelSrcLinkReq{TaskID: taskID, CapToken: capToken})
	if err != nil {
		return SrcLink{}, err
	}
	respRaw, err := c.ch.Send(ctx, proto.MethodTaskSrcLink, payload)
	if err != nil {
		return SrcLink{}, fmt.Errorf("agent: channel src-link: %w", err)
	}
	var wire proto.ChannelSrcLinkResp
	if err := json.Unmarshal(respRaw, &wire); err != nil {
		return SrcLink{}, fmt.Errorf("agent: decode channel src-link resp: %w", err)
	}
	return SrcLink{URL: wire.URL, Headers: wire.Headers, Size: wire.Size, Proxied: wire.Proxied}, nil
}

func (c *channelCapClient) TryRapid(ctx context.Context, taskID, capToken string) (bool, error) {
	payload, err := json.Marshal(proto.ChannelTryRapidReq{TaskID: taskID, CapToken: capToken})
	if err != nil {
		return false, err
	}
	respRaw, err := c.ch.Send(ctx, proto.MethodTaskTryRapid, payload)
	if err != nil {
		return false, fmt.Errorf("agent: channel try-rapid: %w", err)
	}
	var wire proto.ChannelTryRapidResp
	if err := json.Unmarshal(respRaw, &wire); err != nil {
		return false, fmt.Errorf("agent: decode channel try-rapid resp: %w", err)
	}
	return wire.Rapid, nil
}

func (c *channelCapClient) Progress(ctx context.Context, taskID, capToken string, pct int, detail string) error {
	payload, err := json.Marshal(proto.ChannelProgressReq{TaskID: taskID, CapToken: capToken, Progress: pct, Detail: detail})
	if err != nil {
		return err
	}
	if _, err := c.ch.Send(ctx, proto.MethodTaskProgress, payload); err != nil {
		return fmt.Errorf("agent: channel progress: %w", err)
	}
	return nil
}

func (c *channelCapClient) Status(ctx context.Context, taskID, capToken, status, detail string) error {
	payload, err := json.Marshal(proto.ChannelStatusReq{TaskID: taskID, CapToken: capToken, Status: status, Detail: detail})
	if err != nil {
		return err
	}
	if _, err := c.ch.Send(ctx, proto.MethodTaskStatus, payload); err != nil {
		return fmt.Errorf("agent: channel status: %w", err)
	}
	return nil
}

// RenewCap is the reverse-mode leg of CapClient.RenewCap (design cap-renewal):
// it sends the SAME re-authorized op the HTTP leg does, just as a channel
// frame — server-side, both funnel into agentTaskRenewCapCore.
func (c *channelCapClient) RenewCap(ctx context.Context, taskID, capToken string) (string, int64, error) {
	payload, err := json.Marshal(proto.ChannelRenewCapReq{TaskID: taskID, CapToken: capToken})
	if err != nil {
		return "", 0, err
	}
	respRaw, err := c.ch.Send(ctx, proto.MethodTaskRenewCap, payload)
	if err != nil {
		return "", 0, fmt.Errorf("agent: channel renew cap: %w", err)
	}
	var wire proto.ChannelRenewCapResp
	if err := json.Unmarshal(respRaw, &wire); err != nil {
		return "", 0, fmt.Errorf("agent: decode channel renew cap resp: %w", err)
	}
	return wire.CapToken, wire.ExpiresAt, nil
}

// PutContent streams body to the destination over the channel in three legs:
// begin (opens the stream, re-verifies the cap server-side before any byte is
// accepted), a sequence of ack'd chunk requests, and end (flushes EOF to
// op.Put and returns the final outcome).
//
// Flow control / bounded memory: each chunk is a REQUEST the agent waits for an
// ack on before sending the next, so exactly one chunk (≤ proto.MaxChannelChunkBytes)
// is ever in flight, and the server writes each chunk straight into op.Put's
// pipe — a huge file is never buffered whole on either side. AEAD integrity is
// per-frame (design R2.5): a tampered/injected/reordered chunk fails the
// channel's AEAD open and tears the whole channel down before op.Put sees it.
//
// onPct (if non-nil) is fed through the same throttled progress reader the HTTP
// upload uses, so progress reporting is identical across transports.
func (c *channelCapClient) PutContent(ctx context.Context, taskID, capToken string, body io.Reader, size int64, onPct func(pct int)) error {
	streamID := randString(streamChunkIDLen)

	beginPayload, err := json.Marshal(proto.ChannelPutBeginReq{TaskID: taskID, CapToken: capToken, StreamID: streamID, Size: size})
	if err != nil {
		return err
	}
	if _, err := c.ch.Send(ctx, proto.MethodTaskPutBegin, beginPayload); err != nil {
		return fmt.Errorf("agent: channel put begin: %w", err)
	}

	pr := newProgressReader(body, size, onPct)
	buf := make([]byte, proto.MaxChannelChunkBytes)
	var seq uint64
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			chunkPayload, err := json.Marshal(proto.ChannelPutChunkReq{StreamID: streamID, Seq: seq, Data: buf[:n]})
			if err != nil {
				return err
			}
			if _, err := c.ch.Send(ctx, proto.MethodTaskPutChunk, chunkPayload); err != nil {
				return fmt.Errorf("agent: channel put chunk %d: %w", seq, err)
			}
			seq++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("agent: read source for channel upload: %w", readErr)
		}
	}

	endPayload, err := json.Marshal(proto.ChannelPutEndReq{StreamID: streamID})
	if err != nil {
		return err
	}
	if _, err := c.ch.Send(ctx, proto.MethodTaskPutEnd, endPayload); err != nil {
		return fmt.Errorf("agent: channel put end: %w", err)
	}
	return nil
}
