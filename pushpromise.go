package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FramePushPromise FrameType = 0x5

var _ Frame = &PushPromise{}

// PushPromise represents a PUSH_PROMISE frame (RFC 7540 Section 6.6).
//
// PUSH_PROMISE is used to notify the peer of a stream the sender intends
// to initiate. It carries the promised stream ID and a header block fragment.
type PushPromise struct {
	header []byte // header block fragment
	stream uint32
	pad    bool
	ended  bool
}

// Type returns FramePushPromise.
func (pp *PushPromise) Type() FrameType {
	return FramePushPromise
}

// Reset clears all PushPromise fields.
func (pp *PushPromise) Reset() {
	pp.pad = false
	pp.ended = false
	pp.stream = 0
	pp.header = pp.header[:0]
}

// SetHeader replaces the header block fragment with h.
func (pp *PushPromise) SetHeader(h []byte) {
	pp.header = append(pp.header[:0], h...)
}

// Write appends b to the header block fragment, implementing io.Writer.
func (pp *PushPromise) Write(b []byte) (int, error) {
	n := len(b)
	pp.header = append(pp.header, b...)
	return n, nil
}

// Stream returns the promised stream ID.
func (pp *PushPromise) Stream() uint32 {
	return pp.stream
}

// SetStream sets the promised stream ID.
func (pp *PushPromise) SetStream(stream uint32) {
	pp.stream = stream & (1<<31 - 1)
}

// EndHeaders reports whether the END_HEADERS flag is set.
func (pp *PushPromise) EndHeaders() bool {
	return pp.ended
}

// SetEndHeaders sets or clears the END_HEADERS flag.
func (pp *PushPromise) SetEndHeaders(value bool) {
	pp.ended = value
}

// Padding reports whether the PADDED flag is set.
func (pp *PushPromise) Padding() bool {
	return pp.pad
}

// SetPadding enables or disables padding for the PUSH_PROMISE frame.
func (pp *PushPromise) SetPadding(value bool) {
	pp.pad = value
}

// Deserialize reads a PUSH_PROMISE frame from the given frame header payload.
func (pp *PushPromise) Deserialize(fr *FrameHeader) error {
	payload := fr.payload

	if fr.Flags().Has(FlagPadded) {
		pp.pad = true
		var err error
		payload, err = http2utils.CutPadding(payload, fr.Len())
		if err != nil {
			return err
		}
	}

	if len(payload) < 4 {
		return ErrMissingBytes
	}

	pp.stream = http2utils.BytesToUint32(payload) & (1<<31 - 1)
	pp.SetHeader(payload[4:])
	pp.ended = fr.Flags().Has(FlagEndHeaders)

	return nil
}

// Serialize writes the PUSH_PROMISE payload into the frame header.
func (pp *PushPromise) Serialize(fr *FrameHeader) {
	payload := http2utils.AppendUint32Bytes(fr.payload[:0], pp.stream)
	payload = append(payload, pp.header...)

	if pp.pad {
		fr.SetFlags(fr.Flags().Add(FlagPadded))
		payload = http2utils.AddPadding(payload)
	}

	if pp.ended {
		fr.SetFlags(fr.Flags().Add(FlagEndHeaders))
	}

	fr.payload = payload
}
