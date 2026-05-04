package serve

import (
	"fmt"
	"net/http"
	"strings"
)

type textSSEStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newTextSSEStream(w http.ResponseWriter) (*textSSEStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &textSSEStream{w: w, flusher: flusher}, nil
}

func (s *textSSEStream) Send(eventName, data string) error {
	if eventName != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", eventName); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		if _, err := fmt.Fprintf(s.w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(s.w, "\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
