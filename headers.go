package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FrameHeaders FrameType = 0x1

var (
	_ Frame            = &Headers{}
	_ FrameWithHeaders = &Headers{}
)

// FrameWithHeaders is implemented by frame types that carry a header block fragment.
type FrameWithHeaders interface {
	Headers() []byte
}

// Headers represents a HEADERS frame (RFC 7540 Section 6.2).
//
// HEADERS frames open a stream and carry a header block fragment.
// They support optional padding, priority information, and stream termination.
type Headers struct {
	rawHeaders []byte // this field is used to store uncompleted headers.
	stream     uint32
	hasPadding bool
	weight     uint8
	endStream  bool
	endHeaders bool
	priority   bool
}

// Reset clears all HEADERS fields and the raw header buffer.
func (h *Headers) Reset() {
	h.hasPadding = false
	h.stream = 0
	h.weight = 0
	h.endStream = false
	h.endHeaders = false
	h.priority = false
	h.rawHeaders = h.rawHeaders[:0]
}

// CopyTo copies h fields to h2.
func (h *Headers) CopyTo(h2 *Headers) {
	h2.hasPadding = h.hasPadding
	h2.stream = h.stream
	h2.weight = h.weight
	h2.endStream = h.endStream
	h2.endHeaders = h.endHeaders
	h2.priority = h.priority
	h2.rawHeaders = append(h2.rawHeaders[:0], h.rawHeaders...)
}

// Type returns FrameHeaders.
func (h *Headers) Type() FrameType {
	return FrameHeaders
}

// Headers returns the raw header block fragment bytes.
func (h *Headers) Headers() []byte {
	return h.rawHeaders
}

// SetHeaders replaces the raw header block fragment with b.
func (h *Headers) SetHeaders(b []byte) {
	h.rawHeaders = append(h.rawHeaders[:0], b...)
}

// AppendRawHeaders appends b to the raw headers.
func (h *Headers) AppendRawHeaders(b []byte) {
	h.rawHeaders = append(h.rawHeaders, b...)
}

// AppendHeaderField encodes hf and appends it to the raw headers.
func (h *Headers) AppendHeaderField(hp *HPACK, hf *HeaderField, store bool) {
	h.rawHeaders = hp.AppendHeader(h.rawHeaders, hf, store)
}

// EndStream reports whether the END_STREAM flag is set.
func (h *Headers) EndStream() bool {
	return h.endStream
}

// SetEndStream sets or clears the END_STREAM flag.
func (h *Headers) SetEndStream(value bool) {
	h.endStream = value
}

// EndHeaders reports whether the END_HEADERS flag is set.
func (h *Headers) EndHeaders() bool {
	return h.endHeaders
}

// SetEndHeaders sets or clears the END_HEADERS flag.
func (h *Headers) SetEndHeaders(value bool) {
	h.endHeaders = value
}

// Stream returns the stream dependency (used with PRIORITY flag).
func (h *Headers) Stream() uint32 {
	return h.stream
}

// SetStream sets the stream dependency.
func (h *Headers) SetStream(stream uint32) {
	h.stream = stream
}

// Weight returns the priority weight (used with PRIORITY flag).
func (h *Headers) Weight() byte {
	return h.weight
}

// SetWeight sets the priority weight.
func (h *Headers) SetWeight(w byte) {
	h.weight = w
}

// Padding reports whether the PADDED flag is set.
func (h *Headers) Padding() bool {
	return h.hasPadding
}

// SetPadding enables or disables padding.
func (h *Headers) SetPadding(value bool) {
	h.hasPadding = value
}

// Priority reports whether the PRIORITY flag is set.
func (h *Headers) Priority() bool {
	return h.priority
}

// SetPriority enables or disables the PRIORITY flag.
func (h *Headers) SetPriority(value bool) {
	h.priority = value
}

// Deserialize reads a HEADERS frame from the given frame header payload.
func (h *Headers) Deserialize(frh *FrameHeader) error {
	flags := frh.Flags()
	payload := frh.payload

	if flags.Has(FlagPadded) {
		h.hasPadding = true
		var err error
		payload, err = http2utils.CutPadding(payload, len(payload))
		if err != nil {
			return err
		}
	}

	if flags.Has(FlagPriority) {
		if len(payload) < 5 { // 4 (stream) + 1 (weight)
			return ErrMissingBytes
		}
		h.priority = true
		h.stream = http2utils.BytesToUint32(payload) & (1<<31 - 1)
		h.weight = payload[4]
		payload = payload[5:]
	}

	h.endStream = flags.Has(FlagEndStream)
	h.endHeaders = flags.Has(FlagEndHeaders)
	h.rawHeaders = append(h.rawHeaders, payload...)

	return nil
}

// Serialize writes the HEADERS payload into the frame header.
func (h *Headers) Serialize(frh *FrameHeader) {
	if h.endStream {
		frh.SetFlags(
			frh.Flags().Add(FlagEndStream))
	}

	if h.endHeaders {
		frh.SetFlags(
			frh.Flags().Add(FlagEndHeaders))
	}

	if h.priority {
		frh.SetFlags(
			frh.Flags().Add(FlagPriority))
	}

	payload := frh.payload[:0]
	if h.priority {
		n := 5 + len(h.rawHeaders)
		payload = http2utils.Resize(payload, n)
		http2utils.Uint32ToBytes(payload[0:4], h.stream)
		payload[4] = h.weight
		copy(payload[5:], h.rawHeaders)
	} else {
		payload = append(payload, h.rawHeaders...)
	}

	if h.hasPadding {
		frh.SetFlags(
			frh.Flags().Add(FlagPadded))
		payload = http2utils.AddPadding(payload)
	}

	frh.payload = payload
}
