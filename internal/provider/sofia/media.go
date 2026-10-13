package sofia

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"cnm/internal/provider"
	"cnm/internal/tsmux"
)

// Codec of a video elementary stream.
type Codec int

const (
	CodecUnknown Codec = iota
	CodecH264
	CodecH265
)

func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "H.264"
	case CodecH265:
		return "H.265"
	}
	return "unbekannt"
}

// Frame is one media frame extracted from the DVRIP stream.
type Frame struct {
	Type    byte // 0xFC/0xFE I-frame, 0xFD P-frame, 0xFA audio, 0xF9 info
	Codec   Codec
	Payload []byte
	// I-frame header fields (0 for other frames): frame rate and the
	// recording wall clock (packed, see UnpackTime).
	FPS   int
	Stamp uint32
}

// IsVideo reports whether the frame carries video.
func (f Frame) IsVideo() bool { return f.Type == 0xFC || f.Type == 0xFE || f.Type == 0xFD }

// FrameParser reassembles frames from DVRIP media packet payloads. Frames may
// span several packets and a packet may contain several frames (
// fragmentation may cross packet boundaries). On garbage it resynchronises on
// the next valid frame signature.
type FrameParser struct {
	buf       []byte
	lastCodec Codec
	Resyncs   int
}

const maxFrameBuffer = 8 << 20

// codecFromByte reads the codec from the low nibble of the I-frame type
// byte (bits 4-7 carry the high bits of width/8 and height/8).
func codecFromByte(b byte) Codec {
	switch b & 0x0F {
	case 0x02:
		return CodecH264
	case 0x03:
		return CodecH265
	}
	return CodecUnknown
}

func validType(t byte) bool {
	switch t {
	case 0xFC, 0xFD, 0xFE, 0xFA, 0xF9:
		return true
	}
	return false
}

// Push appends packet payload bytes and returns all complete frames.
func (p *FrameParser) Push(data []byte) ([]Frame, error) {
	p.buf = append(p.buf, data...)
	var out []Frame
	for {
		if len(p.buf) < 8 {
			break
		}
		if !(p.buf[0] == 0 && p.buf[1] == 0 && p.buf[2] == 1 && validType(p.buf[3])) {
			if !p.resync() {
				break
			}
			continue
		}
		var hdr, size int
		t := p.buf[3]
		switch t {
		case 0xFC, 0xFE:
			if len(p.buf) < 16 {
				return out, nil
			}
			hdr = 16
			size = int(binary.LittleEndian.Uint32(p.buf[12:16]))
		case 0xFD:
			hdr = 8
			size = int(binary.LittleEndian.Uint32(p.buf[4:8]))
		case 0xFA, 0xF9:
			hdr = 8
			size = int(binary.LittleEndian.Uint16(p.buf[6:8]))
		}
		if size < 0 || size > maxFrameBuffer {
			// implausible length → treat as corruption
			p.buf = p.buf[1:]
			p.Resyncs++
			continue
		}
		if len(p.buf) < hdr+size {
			if len(p.buf) > maxFrameBuffer {
				return out, errors.New("dvrip: frame buffer overflow")
			}
			break
		}
		f := Frame{Type: t, Payload: append([]byte(nil), p.buf[hdr:hdr+size]...)}
		if t == 0xFC || t == 0xFE {
			if c := codecFromByte(p.buf[4]); c != CodecUnknown {
				p.lastCodec = c
			}
			f.FPS = int(p.buf[5] & 0x1F)
			f.Stamp = binary.LittleEndian.Uint32(p.buf[8:12])
		}
		if f.IsVideo() {
			f.Codec = p.lastCodec
			if f.Codec == CodecUnknown {
				f.Codec = sniffCodec(f.Payload)
				if f.Codec != CodecUnknown {
					p.lastCodec = f.Codec
				}
			}
		}
		out = append(out, f)
		p.buf = p.buf[hdr+size:]
	}
	if len(p.buf) == 0 {
		p.buf = nil // release backing array
	}
	return out, nil
}

// resync drops bytes until the next plausible frame signature.
func (p *FrameParser) resync() bool {
	p.Resyncs++
	for i := 1; i+3 < len(p.buf); i++ {
		if p.buf[i] == 0 && p.buf[i+1] == 0 && p.buf[i+2] == 1 && validType(p.buf[i+3]) {
			p.buf = p.buf[i:]
			return true
		}
	}
	// keep the last 3 bytes – a signature may straddle the boundary
	if len(p.buf) > 3 {
		p.buf = append(p.buf[:0], p.buf[len(p.buf)-3:]...)
	}
	return false
}

// ---- Annex-B helpers -----------------------------------------------------------------

// nalUnits returns (offset of start code, nal header offset) pairs.
func nalUnits(b []byte) [][2]int {
	var out [][2]int
	for i := 0; i+3 <= len(b); {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				out = append(out, [2]int{i, i + 3})
				i += 3
				continue
			}
			if i+3 < len(b) && b[i+2] == 0 && b[i+3] == 1 {
				out = append(out, [2]int{i, i + 4})
				i += 4
				continue
			}
		}
		i++
	}
	return out
}

func sniffCodec(b []byte) Codec {
	for _, n := range nalUnits(b) {
		if n[1] >= len(b) {
			continue
		}
		h := b[n[1]]
		if h&0x80 != 0 {
			continue
		}
		t264 := h & 0x1F
		t265 := (h >> 1) & 0x3F
		if t265 == 32 || t265 == 33 || t265 == 34 {
			return CodecH265
		}
		if t264 == 7 || t264 == 8 || t264 == 5 {
			return CodecH264
		}
	}
	return CodecUnknown
}

// keyframeStart returns the offset of the first parameter set (SPS for
// H.264, VPS for H.265) inside an I-frame payload, or -1.
func keyframeStart(b []byte, c Codec) int {
	for _, n := range nalUnits(b) {
		if n[1] >= len(b) {
			continue
		}
		h := b[n[1]]
		switch c {
		case CodecH264:
			if h&0x1F == 7 {
				return n[0]
			}
		case CodecH265:
			if (h>>1)&0x3F == 32 {
				return n[0]
			}
		}
	}
	return -1
}

// FrameSink receives clean access units: the first one starts with a
// parameter set (4-byte start code) followed by the keyframe.
type FrameSink func(au []byte, key bool, codec Codec) error

// AnnexBWriter gates video frames: everything before the first usable
// keyframe is dropped (resynchronisation after connect), leading NAL units
// before the parameter set are cut, and a codec change ends the stream.
type AnnexBWriter struct {
	sink    FrameSink
	codec   Codec
	started bool
}

// NewAnnexBWriter writes the raw elementary stream to w (used by tests and
// for debugging; the bridge uses NewFrameWriter with an MPEG-TS sink).
func NewAnnexBWriter(w io.Writer) *AnnexBWriter {
	return &AnnexBWriter{sink: func(au []byte, _ bool, _ Codec) error { _, err := w.Write(au); return err }}
}

// NewFrameWriter delivers gated access units to sink.
func NewFrameWriter(sink FrameSink) *AnnexBWriter { return &AnnexBWriter{sink: sink} }

// ErrCodecChanged ends a stream when the device switches codec mid-stream.
var ErrCodecChanged = errors.New("dvrip: codec changed")

// Write emits a video frame. Non-video frames are ignored.
func (a *AnnexBWriter) Write(f Frame) error {
	if !f.IsVideo() || len(f.Payload) == 0 {
		return nil
	}
	key := f.Type == 0xFC || f.Type == 0xFE
	if !a.started {
		if !key || f.Codec == CodecUnknown {
			return nil
		}
		off := keyframeStart(f.Payload, f.Codec)
		if off < 0 {
			return nil
		}
		a.codec = f.Codec
		a.started = true
		data := f.Payload[off:]
		if !bytes.HasPrefix(data, []byte{0, 0, 0, 1}) {
			data = append([]byte{0}, data...) // 3-byte start code → 4 bytes
		}
		return a.sink(data, true, a.codec)
	}
	if f.Codec != CodecUnknown && f.Codec != a.codec {
		return ErrCodecChanged
	}
	return a.sink(f.Payload, key, a.codec)
}

// Started reports whether a keyframe has been emitted.
func (a *AnnexBWriter) Started() bool { return a.started }

// Codec returns the detected codec.
func (a *AnnexBWriter) Codec() Codec { return a.codec }

// ---- legacy two-socket live session ------------------------------------------------------

// mediaIdle is the maximum time without media data before a stream is considered dead.
const mediaIdle = 15 * time.Second

// openLegacyMedia performs Claim (media connection) + Start (control connection).
// It returns the media connection, its reader and packets that arrived early.
func openLegacyMedia(ctx context.Context, ctl *Control, host string, port int, channel int, main bool) (net.Conn, *bufio.Reader, [][]byte, error) {
	mconn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return nil, nil, nil, provider.ClassifyNetErr(err, "DVRIP-Medienverbindung")
	}
	stop := context.AfterFunc(ctx, func() { mconn.Close() })
	defer stop()
	rd := bufio.NewReaderSize(mconn, 256<<10)
	// 1. Claim on the media connection
	_ = mconn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := mconn.Write(encodePacket(ctl.Session(), 0, MsgMonitorClaim, monitorCmd(ctl.Session(), "Claim", channel, main))); err != nil {
		mconn.Close()
		return nil, nil, nil, provider.ClassifyNetErr(err, "DVRIP-Claim")
	}
	var early [][]byte
	for {
		h, p, err := readPacket(rd)
		if err != nil {
			mconn.Close()
			return nil, nil, nil, provider.ClassifyNetErr(err, "DVRIP-Claim")
		}
		if h.MsgID == MsgMonitorData {
			early = append(early, p) // some firmware starts streaming immediately
			continue
		}
		if h.MsgID != MsgMonitorClaimRsp {
			continue
		}
		r, err := decodeResponse(p)
		if err != nil || !r.OK() {
			mconn.Close()
			return nil, nil, nil, provider.NewError(provider.KindProtocol, err, "DVRIP-Claim abgelehnt (Code %d)", r.Ret())
		}
		break
	}
	_ = mconn.SetDeadline(time.Time{})
	// 2. Start on the control connection
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	r, err := ctl.RequestRaw(sctx, MsgMonitor, MsgMonitorRsp, monitorCmd(ctl.Session(), "Start", channel, main))
	cancel()
	if err != nil {
		mconn.Close()
		return nil, nil, nil, err
	}
	if !r.OK() {
		mconn.Close()
		return nil, nil, nil, provider.NewError(provider.KindProtocol, nil, "DVRIP-Start abgelehnt (Code %d)", r.Ret())
	}
	return mconn, rd, early, nil
}

// LegacyLive runs the two-socket sequence and returns a reader with an
// MPEG-TS stream (one PES per frame). Closing the reader stops the stream on the device.
func LegacyLive(ctx context.Context, ctl *Control, host string, port int, channel int, main bool) (io.ReadCloser, error) {
	mconn, rd, early, err := openLegacyMedia(ctx, ctl, host, port, channel, main)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	s := &liveStream{pr: pr, conn: mconn, ctl: ctl, channel: channel, main: main}
	go s.pump(rd, pw, early)
	return s, nil
}

type liveStream struct {
	pr      *io.PipeReader
	conn    net.Conn
	ctl     *Control
	channel int
	main    bool
	once    sync.Once
}

func (s *liveStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *liveStream) Close() error {
	s.once.Do(func() {
		s.pr.Close()
		s.conn.Close()
		// tell the device to stop; fire-and-forget so an unreachable device never blocks
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = s.ctl.RequestRaw(ctx, MsgMonitor, MsgMonitorRsp, monitorCmd(s.ctl.Session(), "Stop", s.channel, s.main))
		}()
	})
	return nil
}

func (s *liveStream) pump(rd *bufio.Reader, pw *io.PipeWriter, early [][]byte) {
	var parser FrameParser
	var mux *tsmux.Muxer
	start := time.Now()
	out := NewFrameWriter(func(au []byte, key bool, codec Codec) error {
		if mux == nil {
			st := byte(tsmux.StreamTypeH264)
			if codec == CodecH265 {
				st = tsmux.StreamTypeH265
			}
			mux = tsmux.New(pw, st)
		}
		// arrival time based timestamps (90 kHz), offset to stay positive
		pts := uint64(time.Since(start).Microseconds())*9/100 + 90000
		return mux.WriteFrame(au, pts, key)
	})
	handle := func(p []byte) error {
		frames, err := parser.Push(p)
		if err != nil {
			return err
		}
		for _, f := range frames {
			if err := out.Write(f); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range early {
		if err := handle(p); err != nil {
			pw.CloseWithError(err)
			s.conn.Close()
			return
		}
	}
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(mediaIdle))
		h, p, err := readPacket(rd)
		if err != nil {
			pw.CloseWithError(fmt.Errorf("dvrip media: %w", err))
			s.conn.Close()
			return
		}
		if h.MsgID != MsgMonitorData {
			continue
		}
		if err := handle(p); err != nil {
			pw.CloseWithError(err)
			s.conn.Close()
			return
		}
		select {
		case <-s.ctl.Done():
			pw.CloseWithError(errors.New("dvrip: control connection lost"))
			s.conn.Close()
			return
		default:
		}
	}
}

// ---- live mode detection --------------------------------------------------------------------

// probeWindow is how long detection waits for the first media packet.
const probeWindow = 4 * time.Second

// ProbeModern checks whether the device streams on a single socket (claim and
// start on the login connection) – the variant go2rtc's dvrip source uses.
func ProbeModern(ctx context.Context, host string, port int, user, pass string, channel int) (bool, error) {
	conn, err := provider.Dial(ctx, host, port, 5*time.Second)
	if err != nil {
		return false, provider.ClassifyNetErr(err, "DVRIP")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	rd := bufio.NewReader(conn)
	lr, err := login(conn, rd, user, pass, 8*time.Second)
	if err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Now().Add(probeWindow + 4*time.Second))
	if _, err := conn.Write(encodePacket(lr.Session, 1, MsgMonitorClaim, monitorCmd(lr.Session, "Claim", channel, false))); err != nil {
		return false, nil
	}
	deadline := time.Now().Add(probeWindow)
	claimed := false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		h, p, err := readPacket(rd)
		if err != nil {
			return false, nil
		}
		switch h.MsgID {
		case MsgMonitorClaimRsp:
			r, err := decodeResponse(p)
			if err != nil || !r.OK() {
				return false, nil
			}
			claimed = true
			if _, err := conn.Write(encodePacket(lr.Session, 2, MsgMonitor, monitorCmd(lr.Session, "Start", channel, false))); err != nil {
				return false, nil
			}
		case MsgMonitorData:
			if claimed && len(p) > 0 {
				_, _ = conn.Write(encodePacket(lr.Session, 3, MsgMonitor, monitorCmd(lr.Session, "Stop", channel, false)))
				return true, nil
			}
		}
	}
	return false, nil
}

// ProbeLegacy checks the two-socket variant using an established control
// connection: success means media packets arrive on the separate connection.
func ProbeLegacy(ctx context.Context, ctl *Control, host string, port int, channel int) bool {
	ctx, cancel := context.WithTimeout(ctx, probeWindow+10*time.Second)
	defer cancel()
	mconn, rd, early, err := openLegacyMedia(ctx, ctl, host, port, channel, false)
	if err != nil {
		return false
	}
	defer func() {
		mconn.Close()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_, _ = ctl.RequestRaw(sctx, MsgMonitor, MsgMonitorRsp, monitorCmd(ctl.Session(), "Stop", channel, false))
	}()
	if len(early) > 0 {
		return true
	}
	stop := context.AfterFunc(ctx, func() { mconn.Close() })
	defer stop()
	deadline := time.Now().Add(probeWindow)
	for time.Now().Before(deadline) {
		_ = mconn.SetReadDeadline(deadline)
		h, p, err := readPacket(rd)
		if err != nil {
			return false
		}
		if h.MsgID == MsgMonitorData && len(p) > 0 {
			return true
		}
	}
	return false
}
