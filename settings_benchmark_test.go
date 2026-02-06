package http2

import "testing"

func BenchmarkSettingsEncodeDefaultLike(b *testing.B) {
	st := &Settings{
		tableSize:  defaultHeaderTableSize,
		maxStreams: defaultConcurrentStreams,
		windowSize: defaultWindowSize,
		windowSet:  true,
		frameSize:  defaultDataFrameSize,
		headerSize: 0,
	}

	b.ReportAllocs()
	for b.Loop() {
		st.Encode()
	}
}

func BenchmarkSettingsEncodeAllFields(b *testing.B) {
	st := &Settings{
		tableSize:  1234,
		enablePush: true,
		maxStreams: 200,
		windowSize: 1<<20 - 1,
		windowSet:  true,
		frameSize:  1 << 16,
		headerSize: 8192,
	}

	b.ReportAllocs()
	for b.Loop() {
		st.Encode()
	}
}
