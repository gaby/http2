package http2

import (
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

type StreamState int8

const (
	StreamStateIdle StreamState = iota
	StreamStateReserved
	StreamStateOpen
	StreamStateHalfClosed
	StreamStateClosed
)

func (ss StreamState) String() string {
	switch ss {
	case StreamStateIdle:
		return "Idle"
	case StreamStateReserved:
		return "Reserved"
	case StreamStateOpen:
		return "Open"
	case StreamStateHalfClosed:
		return "HalfClosed"
	case StreamStateClosed:
		return "Closed"
	}

	return "IDK"
}

type Stream struct {
	id                   uint32
	window               int64
	recvWindowSize       int32
	sendWindow           int64
	pendingData          []byte
	pendingDataEndStream bool
	pendingMu            sync.Mutex
	responseStarted      bool
	responseEnded        bool
	state                StreamState
	ctx                  *fasthttp.RequestCtx
	scheme               []byte
	previousHeaderBytes  []byte

	// keeps track of the number of header blocks received
	headerBlockNum int

	// original type
	origType         FrameType
	startedAt        time.Time
	headersFinished  bool
	pendingEndStream bool

	// Header validation tracking
	regularHeaderSeen bool
	seenMethod        bool
	seenScheme        bool
	seenPath          bool
	seenAuthority     bool
	isConnect         bool
}

var streamPool = sync.Pool{
	New: func() any {
		return &Stream{}
	},
}

func NewStream(id uint32, recvWin, sendWin int32) *Stream {
	strm := streamPool.Get().(*Stream)
	strm.id = id
	strm.window = int64(recvWin)
	strm.recvWindowSize = recvWin
	strm.sendWindow = int64(sendWin)
	strm.pendingData = strm.pendingData[:0]
	strm.pendingDataEndStream = false
	strm.responseStarted = false
	strm.responseEnded = false
	strm.state = StreamStateIdle
	strm.headersFinished = false
	strm.pendingEndStream = false
	strm.startedAt = time.Time{}
	strm.previousHeaderBytes = strm.previousHeaderBytes[:0]
	strm.ctx = nil
	strm.scheme = []byte("https")
	strm.origType = 0
	strm.headerBlockNum = 0
	strm.regularHeaderSeen = false
	strm.seenMethod = false
	strm.seenScheme = false
	strm.seenPath = false
	strm.seenAuthority = false
	strm.isConnect = false

	return strm
}

func (s *Stream) ID() uint32 {
	return s.id
}

func (s *Stream) SetID(id uint32) {
	s.id = id
}

func (s *Stream) State() StreamState {
	return s.state
}

func (s *Stream) SetState(state StreamState) {
	s.state = state
}

func (s *Stream) Window() int32 {
	return int32(s.window)
}

func (s *Stream) SetWindow(win int32) {
	s.window = int64(win)
	s.recvWindowSize = win
}

func (s *Stream) IncrWindow(win int32) {
	s.window += int64(win)
}

func (s *Stream) SendWindow() int32 {
	return int32(s.sendWindow)
}

func (s *Stream) SetSendWindow(win int32) {
	s.sendWindow = int64(win)
}

func (s *Stream) IncrSendWindow(win int32) {
	s.sendWindow += int64(win)
}

func (s *Stream) Ctx() *fasthttp.RequestCtx {
	return s.ctx
}

func (s *Stream) SetData(ctx *fasthttp.RequestCtx) {
	s.ctx = ctx
}
