package http2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsSerializeDeserialize(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	st := AcquireFrame(FrameSettings).(*Settings)
	st.Reset()
	st.SetHeaderTableSize(1234)
	st.SetPush(true)
	st.SetMaxConcurrentStreams(10)
	st.SetMaxWindowSize(65535)
	st.SetMaxFrameSize(1<<15 + 1)
	st.SetMaxHeaderListSize(2048)

	fr.SetBody(st)
	st.Serialize(fr)

	var decoded Settings
	err := decoded.Deserialize(fr)
	require.NoError(t, err)

	require.Equal(t, uint32(1234), decoded.HeaderTableSize())
	require.True(t, decoded.Push())
	require.Equal(t, uint32(10), decoded.MaxConcurrentStreams())
	require.Equal(t, uint32(65535), decoded.MaxWindowSize())
	require.Equal(t, uint32(1<<15+1), decoded.MaxFrameSize())
	require.Equal(t, uint32(2048), decoded.MaxHeaderListSize())
}

func TestSettingsDeserializeWrongPayloadLength(t *testing.T) {
	st := &Settings{}
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)
	fr.SetBody(st)

	// Payload not divisible by 6
	fr.payload = make([]byte, 7)
	err := st.Deserialize(fr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong payload")
}

func TestSettingsReadWindowSizeOverflow(t *testing.T) {
	st := &Settings{}

	// MaxWindowSize > 2^31 - 1
	payload := []byte{
		0, byte(MaxWindowSize),
		0xFF, 0xFF, 0xFF, 0xFF, // value = 4294967295 > 2^31-1
	}
	err := st.Read(payload)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SETTINGS_INITIAL_WINDOW_SIZE above maximum")
}

func TestSettingsInvalidValues(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	st := &Settings{}
	fr.SetBody(st)

	// Invalid EnablePush value
	fr.payload = []byte{0, byte(EnablePush), 0, 0, 0, 2}
	require.Error(t, st.Deserialize(fr), "expected error for invalid enable_push")

	// Invalid frame size
	fr.payload = []byte{0, byte(MaxFrameSize), 0, 0, 0, 0}
	require.Error(t, st.Deserialize(fr), "expected error for invalid frame size")

	// ACK with payload should error
	st.SetAck(true)
	fr.SetFlags(FlagAck)
	fr.payload = []byte{0, 0, 0, 0, 0, 0}
	require.Error(t, st.Deserialize(fr), "expected error for ack with payload")
}
