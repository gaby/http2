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
