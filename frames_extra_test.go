package http2

import (
	"bytes"
	"testing"
	"time"

	"github.com/dgrr/http2/http2utils"
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if !decoded.EndStream() || decoded.Padding() {
		t.Fatalf("flags not preserved: end=%v pad=%v", decoded.EndStream(), decoded.Padding())
	}
	if !bytes.Equal(decoded.Data(), []byte("payload")) {
		t.Fatalf("unexpected data: %q", decoded.Data())
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if !decoded.EndHeaders() || !decoded.EndStream() || !decoded.Padding() {
		t.Fatalf("flags missing after decode")
	}
	if decoded.Stream() != 5 || decoded.Weight() != 10 {
		t.Fatalf("stream/weight mismatch")
	}
	if !bytes.Equal(decoded.Headers(), []byte("abc")) {
		t.Fatalf("headers mismatch")
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if !decoded.EndHeaders() {
		t.Fatalf("end headers flag lost")
	}
	if !bytes.Equal(decoded.Headers(), []byte("xyz")) {
		t.Fatalf("payload mismatch")
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if decoded.Stream() != 0x7FFFFFFE { // mask reserved bit
		t.Fatalf("unexpected stream: %d", decoded.Stream())
	}
	if decoded.Weight() != 20 {
		t.Fatalf("weight mismatch")
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if decoded.Increment() != 123 {
		t.Fatalf("increment mismatch: %d", decoded.Increment())
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if !decoded.IsAck() {
		t.Fatalf("ack lost")
	}
	if decoded.DataAsTime().After(time.Now()) {
		t.Fatalf("unexpected timestamp in future")
	}
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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if decoded.stream != 33 {
		t.Fatalf("stream mismatch: %d", decoded.stream)
	}
	if string(decoded.header) != "hdrs" {
		t.Fatalf("header mismatch: %q", decoded.header)
	}
}
