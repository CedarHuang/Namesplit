package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/CedarHuang/Namesplit/internal/event"
)

const (
	eventBatchSize    = 500
	eventBatchEvery   = 50 * time.Millisecond
	eventHeartbeatAge = 5 * time.Second
)

// handleEvents 以 NDJSON 推送事件流：每行一个事件数组。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	snapshot, updates, cancel, err := s.backend.SubscribeEvents(r.Context())
	if err != nil {
		writeError(w, err)

		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeBatches(w, flusher, snapshot)

	flushTicker := time.NewTicker(eventBatchEvery)
	defer flushTicker.Stop()
	heartbeat := time.NewTicker(eventHeartbeatAge)
	defer heartbeat.Stop()

	batch := make([]event.Event, 0, eventBatchSize)

	for {
		select {
		case ev, ok := <-updates:
			if !ok {
				if len(batch) > 0 {
					writeEventBatch(w, flusher, batch)
				}

				return
			}
			batch = append(batch, ev)
			if len(batch) >= eventBatchSize {
				writeEventBatch(w, flusher, batch)
				batch = batch[:0]
			}
		case <-flushTicker.C:
			if len(batch) > 0 {
				writeEventBatch(w, flusher, batch)
				batch = batch[:0]
			}
		case <-heartbeat.C:
			if len(batch) == 0 {
				_, _ = w.Write([]byte("\n"))
				flush(flusher)
			}
		case <-r.Context().Done():
			return
		}
	}
}

// writeBatches 把快照按固定大小分批写出。
func writeBatches(w http.ResponseWriter, flusher http.Flusher, events []event.Event) {
	for start := 0; start < len(events); start += eventBatchSize {
		end := start + eventBatchSize
		if end > len(events) {
			end = len(events)
		}
		writeEventBatch(w, flusher, events[start:end])
	}
}

// writeEventBatch 写出一行事件数组。
func writeEventBatch(w http.ResponseWriter, flusher http.Flusher, events []event.Event) {
	if len(events) == 0 {
		return
	}

	body, err := json.Marshal(events)
	if err != nil {
		return
	}

	_, _ = w.Write(append(body, '\n'))
	flush(flusher)
}

// flush 触发 HTTP 刷新。
func flush(flusher http.Flusher) {
	if flusher != nil {
		flusher.Flush()
	}
}
