package http2

import (
	"bytes"
	"testing"
	"time"

	"github.com/dgrr/http2/http2utils"
	"github.com/stretchr/testify/require"
)

func TestDataFrameSerializeDeserialize(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	data := AcquireFrame(FrameData).(*Data)
	data.SetData([]byte("payload"))
	data.SetEndStream(true)
	data.SetPadding(true)

	fr.SetBody(data)
	data.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded Data
	err := decoded.Deserialize(fr)
	require.NoError(t, err)

	require.True(t, decoded.EndStream(), "end stream flag lost")
	require.False(t, decoded.Padding(), "padding flag should not be set")
	require.True(t, bytes.Equal(decoded.Data(), []byte("payload")), "unexpected data: %q", decoded.Data())
}

func TestHeadersSerializeDeserialize(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetPadding(true)
	h.SetEndHeaders(true)
	h.SetEndStream(true)
	h.SetStream(5)
	h.SetWeight(10)
	h.SetHeaders([]byte("abc"))
	h.priority = true

	fr.SetBody(h)
	fr.SetStream(5)
	h.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded Headers
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.True(t, decoded.EndHeaders(), "end headers flag missing")
	require.True(t, decoded.EndStream(), "end stream flag missing")
	require.True(t, decoded.Padding(), "padding flag missing")
	require.Equal(t, uint32(5), decoded.Stream(), "stream mismatch")
	require.Equal(t, uint8(10), decoded.Weight(), "weight mismatch")
	require.True(t, bytes.Equal(decoded.Headers(), []byte("abc")), "headers mismatch")
}

func TestContinuationRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	cont := AcquireFrame(FrameContinuation).(*Continuation)
	cont.SetEndHeaders(true)
	cont.SetHeader([]byte("xyz"))

	fr.SetBody(cont)
	cont.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded Continuation
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.True(t, decoded.EndHeaders(), "end headers flag lost")
	require.True(t, bytes.Equal(decoded.Headers(), []byte("xyz")), "payload mismatch")
}

func TestPriorityRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	pr := AcquireFrame(FramePriority).(*Priority)
	pr.SetStream(0xFFFFFFFE)
	pr.SetWeight(20)

	fr.SetBody(pr)
	pr.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded Priority
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.Equal(t, uint32(0x7FFFFFFE), decoded.Stream(), "unexpected stream")
	require.Equal(t, uint8(20), decoded.Weight(), "weight mismatch")
}

func TestWindowUpdateRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(123)

	fr.SetBody(wu)
	wu.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded WindowUpdate
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.EqualValues(t, 123, decoded.Increment(), "increment mismatch")
}

func TestPingRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	p := AcquireFrame(FramePing).(*Ping)
	p.SetAck(true)
	p.SetCurrentTime()

	fr.SetBody(p)
	p.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded Ping
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.True(t, decoded.IsAck(), "ack lost")
	require.False(t, decoded.DataAsTime().After(time.Now()), "unexpected timestamp in future")
}

func TestPushPromiseRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	pp := AcquireFrame(FramePushPromise).(*PushPromise)
	pp.SetHeader([]byte("hdrs"))

	fr.SetBody(pp)
	fr.SetStream(33)
	pp.Serialize(fr)
	fr.length = len(fr.payload)

	// synthesize mandatory stream + header bytes
	fr.payload = append(http2utils.AppendUint32Bytes([]byte{}, fr.Stream()), fr.payload...)
	fr.length = len(fr.payload)

	var decoded PushPromise
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.Equal(t, uint32(33), decoded.stream, "stream mismatch")
	require.Equal(t, "hdrs", string(decoded.header), "header mismatch")
}
