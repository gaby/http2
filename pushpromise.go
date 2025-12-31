package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FramePushPromise FrameType = 0x5

var _ Frame = &PushPromise{}

// PushPromise https://tools.ietf.org/html/rfc7540#section-6.6
type PushPromise struct {
	pad    bool
	ended  bool
	stream uint32
	header []byte // header block fragment
}

func (pp *PushPromise) Type() FrameType {
	return FramePushPromise
}

func (pp *PushPromise) Reset() {
	pp.pad = false
	pp.ended = false
	pp.stream = 0
	pp.header = pp.header[:0]
}

func (pp *PushPromise) SetHeader(h []byte) {
	pp.header = append(pp.header[:0], h...)
}

func (pp *PushPromise) Write(b []byte) (int, error) {
	n := len(b)
	pp.header = append(pp.header, b...)
	return n, nil
}

func (pp *PushPromise) Stream() uint32 {
	return pp.stream
}

func (pp *PushPromise) SetStream(stream uint32) {
	pp.stream = stream & (1<<31 - 1)
}

func (pp *PushPromise) EndHeaders() bool {
	return pp.ended
}

func (pp *PushPromise) SetEndHeaders(value bool) {
	pp.ended = value
}

func (pp *PushPromise) Padding() bool {
	return pp.pad
}

func (pp *PushPromise) SetPadding(value bool) {
	pp.pad = value
}

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
