/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

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
