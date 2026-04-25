package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FramePriority FrameType = 0x2

var _ Frame = &Priority{}

// Priority represents a PRIORITY frame (RFC 7540 Section 6.3).
//
// PRIORITY frames specify the sender-advised priority of a stream.
// They carry a stream dependency and a weight value.
type Priority struct {
	stream uint32
	weight byte
}

// Type returns FramePriority.
func (pry *Priority) Type() FrameType {
	return FramePriority
}

// Reset resets priority fields.
func (pry *Priority) Reset() {
	pry.stream = 0
	pry.weight = 0
}

// CopyTo copies the priority state into p.
func (pry *Priority) CopyTo(p *Priority) {
	p.stream = pry.stream
	p.weight = pry.weight
}

// Stream returns the Priority frame stream.
func (pry *Priority) Stream() uint32 {
	return pry.stream
}

// SetStream sets the Priority frame stream.
func (pry *Priority) SetStream(stream uint32) {
	pry.stream = stream & (1<<31 - 1)
}

// Weight returns the Priority frame weight.
func (pry *Priority) Weight() byte {
	return pry.weight
}

// SetWeight sets the Priority frame weight.
func (pry *Priority) SetWeight(w byte) {
	pry.weight = w
}

// Deserialize reads a PRIORITY frame from the given frame header payload.
func (pry *Priority) Deserialize(fr *FrameHeader) (err error) {
	if len(fr.payload) != 5 {
		return newFrameSizeError(fr.Stream(), "priority frame payload must be 5 bytes")
	}

	pry.stream = http2utils.BytesToUint32(fr.payload) & (1<<31 - 1)
	pry.weight = fr.payload[4]
	return nil
}

// Serialize writes the PRIORITY payload into the frame header.
func (pry *Priority) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], pry.stream)
	fr.payload = append(fr.payload, pry.weight)
}
