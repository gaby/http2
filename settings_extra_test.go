package http2

import "testing"

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
	if err := decoded.Deserialize(fr); err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if decoded.HeaderTableSize() != 1234 || !decoded.Push() || decoded.MaxConcurrentStreams() != 10 {
		t.Fatalf("settings not decoded correctly")
	}
	if decoded.MaxWindowSize() != 65535 || decoded.MaxFrameSize() != 1<<15+1 || decoded.MaxHeaderListSize() != 2048 {
		t.Fatalf("unexpected values after decode")
	}
}

func TestSettingsInvalidValues(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	st := &Settings{}
	fr.SetBody(st)

	// Invalid EnablePush value
	fr.payload = []byte{0, byte(EnablePush), 0, 0, 0, 2}
	if err := st.Deserialize(fr); err == nil {
		t.Fatalf("expected error for invalid enable_push")
	}

	// Invalid frame size
	fr.payload = []byte{0, byte(MaxFrameSize), 0, 0, 0, 0}
	if err := st.Deserialize(fr); err == nil {
		t.Fatalf("expected error for invalid frame size")
	}

	// ACK with payload should error
	st.SetAck(true)
	fr.SetFlags(FlagAck)
	fr.payload = []byte{0, 0, 0, 0, 0, 0}
	if err := st.Deserialize(fr); err == nil {
		t.Fatalf("expected error for ack with payload")
	}
}
