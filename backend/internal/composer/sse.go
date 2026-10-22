package composer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteSSE relays chunks to an HTTP client as server-sent events:
//
//	event: delta
//	data: {"type":"delta","attempt":1,"field":"sms_content","delta":"Ayşe, sepet"}
//
// Each event is flushed immediately. It returns when chunks is closed or ctx
// (normally r.Context(), cancelled when the client disconnects) is done.
func WriteSSE(ctx context.Context, w http.ResponseWriter, chunks <-chan MessageChunk) error {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("sse: response does not support flushing: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch, ok := <-chunks:
			if !ok {
				return nil
			}
			data, err := json.Marshal(ch)
			if err != nil {
				return err
			}
			// json.Marshal never emits raw newlines, so one data line is safe.
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ch.Type, data); err != nil {
				return err
			}
			if err := rc.Flush(); err != nil {
				return err
			}
		}
	}
}
