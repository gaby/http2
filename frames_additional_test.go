package http2

import (
	"testing"

	"github.com/dgrr/http2/http2utils"
	"github.com/stretchr/testify/require"
)

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

func TestHeadersCopyAndAppendRaw(t *testing.T) {
	h := &Headers{}
	h.SetPadding(true)
	h.SetEndHeaders(true)
	h.SetEndStream(true)
	h.SetStream(3)
	h.SetWeight(5)
	h.AppendRawHeaders([]byte("abc"))

	h2 := &Headers{}
	h.CopyTo(h2)

	require.True(t, h2.Padding())
	require.True(t, h2.EndHeaders())
	require.True(t, h2.EndStream())
	require.Equal(t, h.Stream(), h2.Stream())
	require.Equal(t, h.Weight(), h2.Weight())
	require.Equal(t, []byte("abc"), h2.Headers())
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
	require.False(t, clonedPing.IsAck())

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

func TestPushPromiseAndRstStreamHelpers(t *testing.T) {
	pp := &PushPromise{}
	_, err := pp.Write([]byte("headers"))
	require.NoError(t, err)

	fr := AcquireFrameHeader()
	fr.SetBody(pp)
	pp.Serialize(fr)

	require.Equal(t, []byte("headers"), fr.payload)

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
