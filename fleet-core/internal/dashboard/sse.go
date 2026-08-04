package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func sseHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		since := int64(0)
		if v := r.URL.Query().Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				since = n
			}
		}

		ch, cancel := hub.Subscribe(r.PathValue("id"), since)
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				body, _ := json.Marshal(e)
				if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
					return // client disconnected mid-write
				}
				flusher.Flush()
			}
		}
	}
}
