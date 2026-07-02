package http2

import "slices"

// Streams is a slice of Stream pointers with helper methods for search and deletion.
type Streams []*Stream

// Search returns the stream with the given ID, or nil if not found.
func (strms *Streams) Search(id uint32) *Stream {
	for _, strm := range *strms {
		if strm.ID() == id {
			return strm
		}
	}
	return nil
}

// Del removes the stream with the given ID from the slice.
func (strms *Streams) Del(id uint32) {
	if len(*strms) == 1 && (*strms)[0].ID() == id {
		*strms = (*strms)[:0]
		return
	}

	for i, strm := range *strms {
		if strm.ID() == id {
			*strms = append((*strms)[:i], (*strms)[i+1:]...)
			return
		}
	}
}

// GetFirstOf returns the first stream with the given origin frame type, or nil.
func (strms Streams) GetFirstOf(frameType FrameType) *Stream {
	for _, strm := range strms {
		if strm.origType == frameType {
			return strm
		}
	}
	return nil
}

func (strms Streams) getPrevious(frameType FrameType) *Stream {
	cnt := 0
	for _, strm := range slices.Backward(strms) {
		if strm.origType == frameType {
			if cnt != 0 {
				return strm
			}
			cnt++
		}
	}
	return nil
}
