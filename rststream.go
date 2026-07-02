package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FrameResetStream FrameType = 0x3

var _ Frame = &RstStream{}

// RstStream represents a RST_STREAM frame (RFC 7540 Section 6.4).
//
// RST_STREAM allows immediate termination of a stream, carrying an error code
// that indicates why the stream is being closed.
type RstStream struct {
	code ErrorCode
}

// Type returns FrameResetStream.
func (rst *RstStream) Type() FrameType {
	return FrameResetStream
}

// Code returns the error code carried by this RST_STREAM frame.
func (rst *RstStream) Code() ErrorCode {
	return rst.code
}

// SetCode sets the error code for the RST_STREAM frame.
func (rst *RstStream) SetCode(code ErrorCode) {
	rst.code = code
}

// Reset zeroes the error code.
func (rst *RstStream) Reset() {
	rst.code = 0
}

// CopyTo copies the RST_STREAM state into r.
func (rst *RstStream) CopyTo(r *RstStream) {
	r.code = rst.code
}

// Error returns the error code as an error value.
func (rst *RstStream) Error() error {
	return rst.code
}

// Deserialize reads a RST_STREAM frame from the given frame header payload.
func (rst *RstStream) Deserialize(fr *FrameHeader) error {
	// RFC 7540 §6.4: a RST_STREAM with a length other than 4 octets MUST be a
	// connection error of type FRAME_SIZE_ERROR.
	if len(fr.payload) != 4 {
		return NewGoAwayError(FrameSizeError, "rst_stream frame payload must be 4 bytes")
	}

	rst.code = ErrorCode(http2utils.BytesToUint32(fr.payload))

	return nil
}

// Serialize writes the RST_STREAM payload into the frame header.
func (rst *RstStream) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], uint32(rst.code))
	fr.length = 4
}
