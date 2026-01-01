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

func TestDataSerializeDeserializeWithPadding(t *testing.T) {
	payload := []byte("payload")

	data := &Data{}
	data.SetData(payload)
	data.SetPadding(true)
	data.SetEndStream(true)

	fr := AcquireFrameHeader()
	fr.SetBody(data)
	data.Serialize(fr)
	require.True(t, fr.Flags().Has(FlagPadded))
	require.True(t, fr.Flags().Has(FlagEndStream))
	require.Greater(t, len(fr.payload), len(payload))

	fr.length = len(fr.payload)

	cloned := &Data{}
	err := cloned.Deserialize(fr)
	require.NoError(t, err)
	require.True(t, cloned.EndStream())
	require.Equal(t, payload, cloned.Data())

	ReleaseFrameHeader(fr)
}

func TestDataCopyToAndLen(t *testing.T) {
	src := &Data{}
	src.SetData([]byte("hello"))
	src.SetPadding(true)
	src.SetEndStream(true)

	dst := &Data{}
	src.CopyTo(dst)

	require.True(t, dst.Padding())
	require.True(t, dst.EndStream())
	require.Equal(t, src.Len(), dst.Len())
	require.Equal(t, src.Data(), dst.Data())
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
	h.SetPriority(true)

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

func TestHeadersPrioritySerializeUsesDependencyStream(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	h := &Headers{}
	h.SetPriority(true)
	h.SetStream(0x7FFFFFFE)
	h.SetWeight(11)
	h.AppendRawHeaders([]byte("abc"))

	fr.SetStream(33)
	fr.SetBody(h)
	h.Serialize(fr)

	require.True(t, fr.Flags().Has(FlagPriority))
	require.Equal(t, []byte{0x7f, 0xff, 0xff, 0xfe}, fr.payload[:4])
	require.Equal(t, byte(11), fr.payload[4])
	require.Equal(t, []byte("abc"), fr.payload[5:])
}

func TestHeadersCopyAndAppendRaw(t *testing.T) {
	h := &Headers{}
	h.SetPadding(true)
	h.SetEndHeaders(true)
	h.SetEndStream(true)
	h.SetStream(3)
	h.SetWeight(5)
	h.SetPriority(true)
	h.AppendRawHeaders([]byte("abc"))

	h2 := &Headers{}
	h.CopyTo(h2)

	require.True(t, h2.Padding())
	require.True(t, h2.EndHeaders())
	require.True(t, h2.EndStream())
	require.True(t, h2.Priority())
	require.Equal(t, h.Stream(), h2.Stream())
	require.Equal(t, h.Weight(), h2.Weight())
	require.Equal(t, []byte("abc"), h2.Headers())
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

func TestContinuationSerializeAndDeserialize(t *testing.T) {
	fr := AcquireFrameHeader()
	cont := &Continuation{}
	fr.SetBody(cont)
	cont.SetHeader([]byte("abc"))
	cont.AppendHeader([]byte("def"))
	cont.SetEndHeaders(true)

	cont.Serialize(fr)
	require.True(t, fr.Flags().Has(FlagEndHeaders))
	require.Equal(t, []byte("abcdef"), fr.payload)

	cloned := &Continuation{}
	err := cloned.Deserialize(fr)
	require.NoError(t, err)
	require.True(t, cloned.EndHeaders())
	require.Equal(t, []byte("abcdef"), cloned.Headers())

	ReleaseFrameHeader(fr)
}

func TestContinuationCopyAndWrite(t *testing.T) {
	original := &Continuation{}
	_, err := original.Write([]byte("headers"))
	require.NoError(t, err)
	original.SetEndHeaders(true)

	cloned := &Continuation{}
	original.CopyTo(cloned)

	require.True(t, cloned.EndHeaders())
	require.Equal(t, original.Headers(), cloned.Headers())
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

func TestPingAndPriorityHelpers(t *testing.T) {
	ping := &Ping{}
	_, err := ping.Write([]byte("abcdefgh"))
	require.NoError(t, err)
	ping.SetAck(true)

	fr := AcquireFrameHeader()
	fr.SetBody(ping)
	ping.Serialize(fr)
	require.Equal(t, ping.Data(), fr.payload)

	clonedPing := &Ping{}
	require.NoError(t, clonedPing.Deserialize(fr))
	require.True(t, clonedPing.IsAck())
	require.Equal(t, ping.Data(), clonedPing.Data())
	ReleaseFrameHeader(fr)

	var target Ping
	clonedPing.CopyTo(&target)
	require.True(t, clonedPing.IsAck())
	require.True(t, target.IsAck())
	require.Equal(t, clonedPing.Data(), target.Data())

	pr := &Priority{}
	pr.SetStream(99)
	pr.SetWeight(7)
	fr = AcquireFrameHeader()
	fr.SetBody(pr)
	pr.Serialize(fr)

	var prCopy Priority
	require.NoError(t, prCopy.Deserialize(fr))
	require.Equal(t, pr.Stream(), prCopy.Stream())
	require.Equal(t, pr.Weight(), prCopy.Weight())
	var prCopy2 Priority
	prCopy.CopyTo(&prCopy2)
	require.Equal(t, prCopy.Stream(), prCopy2.Stream())
	require.Equal(t, prCopy.Weight(), prCopy2.Weight())
	ReleaseFrameHeader(fr)
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

func TestWindowUpdateRejectsZeroIncrement(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.payload = []byte{0, 0, 0, 0}
	fr.length = len(fr.payload)

	wu := &WindowUpdate{}
	fr.SetBody(wu)
	err := wu.Deserialize(fr)
	require.NoError(t, err)
	require.Zero(t, wu.Increment())
}

func TestGoAwaySerializeAndDeserialize(t *testing.T) {
	fr := AcquireFrameHeader()
	ga := &GoAway{}
	fr.SetBody(ga)
	ga.SetStream(123)
	ga.SetCode(EnhanceYourCalm)
	ga.SetData([]byte("debug info"))

	ga.Serialize(fr)
	require.Equal(t, FrameGoAway, fr.Type())

	cloned := &GoAway{}
	fr.length = len(fr.payload)
	err := cloned.Deserialize(fr)
	require.NoError(t, err)
	require.Equal(t, ga.Stream(), cloned.Stream())
	require.Equal(t, ga.Code(), cloned.Code())
	require.Equal(t, ga.Data(), cloned.Data())
	require.Contains(t, cloned.Error(), "Enhance your calm")

	ReleaseFrameHeader(fr)
}

func TestGoAwayCopyHelpers(t *testing.T) {
	ga := &GoAway{}
	ga.SetStream(7)
	ga.SetCode(StreamCanceled)
	ga.SetData([]byte("debug info"))

	var target GoAway
	ga.CopyTo(&target)

	cloned := ga.Copy()
	ga.SetData([]byte("updated"))

	require.Equal(t, uint32(7), target.Stream())
	require.Equal(t, StreamCanceled, target.Code())
	require.Equal(t, []byte("debug info"), target.Data())
	require.Equal(t, target.Stream(), cloned.Stream())
	require.Equal(t, target.Code(), cloned.Code())
	require.Equal(t, target.Data(), cloned.Data())
}

func TestPushPromiseRoundTrip(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	pp := AcquireFrame(FramePushPromise).(*PushPromise)
	pp.SetHeader([]byte("hdrs"))
	pp.SetStream(21)

	fr.SetBody(pp)
	fr.SetStream(33)
	pp.Serialize(fr)
	fr.length = len(fr.payload)

	var decoded PushPromise
	err := decoded.Deserialize(fr)
	require.NoError(t, err)
	require.Equal(t, uint32(21), decoded.stream, "stream mismatch")
	require.Equal(t, "hdrs", string(decoded.header), "header mismatch")
}

func TestPushPromiseAndRstStreamHelpers(t *testing.T) {
	pp := &PushPromise{}
	_, err := pp.Write([]byte("headers"))
	require.NoError(t, err)
	pp.SetStream(13)
	pp.SetEndHeaders(true)

	fr := AcquireFrameHeader()
	fr.SetBody(pp)
	pp.Serialize(fr)

	require.Equal(t, []byte{0, 0, 0, 13}, fr.payload[:4])
	require.True(t, fr.Flags().Has(FlagEndHeaders))
	require.Equal(t, []byte("headers"), fr.payload[4:])

	parsed := &PushPromise{}
	parsedFr := AcquireFrameHeader()
	parsedFr.SetBody(parsed)
	parsedFr.payload = http2utils.AppendUint32Bytes(parsedFr.payload[:0], 11)
	parsedFr.payload = append(parsedFr.payload, []byte("decoded")...)
	parsedFr.length = len(parsedFr.payload)
	parsedFr.SetFlags(parsedFr.Flags().Add(FlagEndHeaders))

	require.NoError(t, parsed.Deserialize(parsedFr))
	require.Equal(t, uint32(11), parsed.stream)
	require.Equal(t, []byte("decoded"), parsed.header)
	require.True(t, parsed.ended)

	ReleaseFrameHeader(fr)
	ReleaseFrameHeader(parsedFr)

	rst := &RstStream{}
	rst.SetCode(StreamCanceled)
	fr = AcquireFrameHeader()
	fr.SetBody(rst)
	rst.Serialize(fr)

	var rstCopy RstStream
	require.NoError(t, rstCopy.Deserialize(fr))
	require.Equal(t, StreamCanceled, rstCopy.Code())

	rst2 := &RstStream{}
	rstCopy.CopyTo(rst2)
	require.Equal(t, rstCopy.Code(), rst2.Code())
	require.Equal(t, StreamCanceled, rstCopy.Error())

	ReleaseFrameHeader(fr)
}

func TestPushPromiseDeserializeWithInsufficientPadding(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetFlags(fr.Flags().Add(FlagPadded))
	fr.payload = []byte{5, 1, 2, 3, 4, 5, 6, 7, 8}
	fr.length = len(fr.payload)

	pp := &PushPromise{}
	fr.SetBody(pp)
	err := pp.Deserialize(fr)
	require.ErrorIs(t, err, ErrMissingBytes)
}
