package http2

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

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
