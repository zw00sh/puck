package server

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// frameSet is an immutable, already-marshalled snapshot of every SSE frame a
// freshly connected client needs in order to render a complete dashboard.
//
// The render loop publishes one of these each tick. A connecting client loads
// the current pointer and writes the bytes straight out: no rebuild, no
// database read, no broadcast to the clients that were already there. That is
// what makes connect O(1) regardless of how expensive the data was to produce.
type frameSet struct {
	hello    []byte
	stats    []byte
	devices  []byte
	cache    []byte
	services []byte
}

// greeting returns the frames to write to a newly attached client, in the order
// the UI wants them (identity first, then data).
func (f *frameSet) greeting() [][]byte {
	if f == nil {
		return nil
	}
	return [][]byte{f.hello, f.stats, f.devices, f.cache, f.services}
}

// sseFrame marshals one named SSE event. A nil/unmarshalable payload yields nil,
// which callers skip.
func sseFrame(event string, data any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload))
}

// changed reports whether a freshly built frame differs from the last one, so
// the render loop can skip broadcasting unchanged data. Cheap because both sides
// are already-marshalled bytes.
func changed(prev, next []byte) bool {
	return next != nil && !bytes.Equal(prev, next)
}
