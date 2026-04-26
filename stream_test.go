package http2

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestStreamsCollectionOperations(t *testing.T) {
	// Build a collection of streams
	var strms Streams
	for _, id := range []uint32{1, 3, 5, 7, 9} {
		s := NewStream(id, 65535, 65535)
		s.origType = FrameHeaders
		strms = append(strms, s)
	}

	// Search existing
	require.NotNil(t, strms.Search(5))
	require.Equal(t, uint32(5), strms.Search(5).ID())

	// Search non-existing
	require.Nil(t, strms.Search(99))

	// GetFirstOf
	first := strms.GetFirstOf(FrameHeaders)
	require.NotNil(t, first)
	require.Equal(t, uint32(1), first.ID())

	// GetFirstOf non-existing type
	require.Nil(t, strms.GetFirstOf(FramePing))

	// Del middle element
	strms.Del(5)
	require.Nil(t, strms.Search(5))
	require.Len(t, strms, 4)

	// Del last element
	strms.Del(9)
	require.Len(t, strms, 3)

	// Del single remaining → trim to 0
	strms = Streams{NewStream(99, 65535, 65535)}
	strms.Del(99)
	require.Len(t, strms, 0)

	// Del non-existing is no-op
	strms = Streams{NewStream(1, 65535, 65535)}
	strms.Del(999)
	require.Len(t, strms, 1)
}

func TestStreamHelpers(t *testing.T) {
	require.Equal(t, "Idle", StreamStateIdle.String())
	require.Equal(t, "Reserved", StreamStateReserved.String())
	require.Equal(t, "Open", StreamStateOpen.String())
	require.Equal(t, "HalfClosed", StreamStateHalfClosed.String())
	require.Equal(t, "Closed", StreamStateClosed.String())
	require.Equal(t, "IDK", StreamState(99).String())

	stream := NewStream(1, 10, 10)
	require.Equal(t, uint32(1), stream.ID())
	require.Equal(t, int32(10), stream.Window())
	require.Equal(t, int32(10), stream.SendWindow())

	stream.SetID(2)
	stream.SetState(StreamStateHalfClosed)
	stream.SetWindow(20)
	stream.IncrWindow(5)
	stream.SetSendWindow(15)
	stream.IncrSendWindow(5)

	ctx := &fasthttp.RequestCtx{}
	stream.SetData(ctx)

	require.Equal(t, uint32(2), stream.ID())
	require.Equal(t, StreamStateHalfClosed, stream.State())
	require.Equal(t, int32(25), stream.Window())
	require.Equal(t, int32(20), stream.SendWindow())
	require.Same(t, ctx, stream.Ctx())
}

func TestNewStreamResetsAllFields(t *testing.T) {
	// First allocation — set some non-default values
	s := NewStream(1, 100, 200)
	s.headersFinished = true
	s.pendingEndStream = true
	s.responseStarted = true
	s.responseEnded = true
	s.isTrailer = true
	s.seenMethod = true
	s.seenScheme = true
	s.seenPath = true
	s.seenAuthority = true
	s.isConnect = true
	s.regularHeaderSeen = true
	s.contentLength = 42
	s.bodyBytesReceived = 42
	s.headerBlockNum = 5
	s.headerListSize = 999
	s.previousHeaderBytes = append(s.previousHeaderBytes, 1, 2, 3)
	s.pendingData = append(s.pendingData, 4, 5, 6)

	// Return to pool via NewStream (which gets from pool)
	streamPool.Put(s)

	// Re-acquire — all fields must be reset
	s2 := NewStream(3, 50, 75)
	require.Equal(t, uint32(3), s2.ID())
	require.Equal(t, int32(50), s2.Window())
	require.Equal(t, int32(75), s2.SendWindow())
	require.Equal(t, StreamStateIdle, s2.State())
	require.False(t, s2.headersFinished)
	require.False(t, s2.pendingEndStream)
	require.False(t, s2.responseStarted)
	require.False(t, s2.responseEnded)
	require.False(t, s2.isTrailer)
	require.False(t, s2.seenMethod)
	require.False(t, s2.seenScheme)
	require.False(t, s2.seenPath)
	require.False(t, s2.seenAuthority)
	require.False(t, s2.isConnect)
	require.False(t, s2.regularHeaderSeen)
	require.Equal(t, int64(-1), s2.contentLength)
	require.Equal(t, int64(0), s2.bodyBytesReceived)
	require.Equal(t, 0, s2.headerBlockNum)
	require.Equal(t, uint32(0), s2.headerListSize)
	require.Empty(t, s2.previousHeaderBytes)
	require.Empty(t, s2.pendingData)
	require.Nil(t, s2.ctx)
}
