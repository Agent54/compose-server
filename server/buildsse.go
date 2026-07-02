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
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/server/errdefs"
)

type buildRegistry struct {
	mu     sync.Mutex
	nextID uint64
	builds map[string]*buildRecord
	order  []string
}

type buildRecord struct {
	id          string
	project     string
	startedAt   time.Time
	finishedAt  *time.Time
	status      string
	success     *bool
	messages    []sseMessage
	subscribers map[int]chan sseMessage
	nextSubID   int
}

type buildSummary struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Success    *bool      `json:"success,omitempty"`
	StreamURL  string     `json:"streamUrl"`
}

type buildEventProcessor struct {
	registry *buildRegistry
	buildID  string
	project  string
}

type buildStreamWriter struct {
	registry *buildRegistry
	buildID  string
	project  string
	stream   string

	mu      sync.Mutex
	pending string
}

func newBuildRegistry() *buildRegistry {
	return &buildRegistry{builds: map[string]*buildRecord{}}
}

func (r *buildRegistry) start(project string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	buildID := strconv.FormatUint(r.nextID, 36)
	now := time.Now().UTC()
	r.builds[buildID] = &buildRecord{
		id:          buildID,
		project:     project,
		startedAt:   now,
		status:      "running",
		subscribers: map[int]chan sseMessage{},
	}
	r.order = append(r.order, buildID)
	r.builds[buildID].messages = append(r.builds[buildID].messages, sseMessage{
		Project: project,
		Stream:  "status",
		Source:  composeapi.ResourceCompose,
		Message: "Build started",
		Time:    now,
	})
	return buildID
}

func (r *buildRegistry) finish(buildID string, success bool) {
	r.mu.Lock()
	record := r.builds[buildID]
	if record == nil {
		r.mu.Unlock()
		return
	}
	if record.finishedAt != nil {
		r.mu.Unlock()
		return
	}

	now := time.Now().UTC()
	record.finishedAt = &now
	record.success = &success
	finalMessage := sseMessage{
		Project: record.project,
		Stream:  "status",
		Source:  composeapi.ResourceCompose,
		Time:    now,
	}
	if success {
		record.status = "succeeded"
		finalMessage.Message = "Build completed"
	} else {
		record.status = "failed"
		finalMessage.Message = "Build failed"
	}
	record.messages = append(record.messages, finalMessage)

	subscribers := make([]chan sseMessage, 0, len(record.subscribers))
	for id, ch := range record.subscribers {
		delete(record.subscribers, id)
		subscribers = append(subscribers, ch)
	}
	r.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- finalMessage:
		default:
		}
		close(ch)
	}
}

func (r *buildRegistry) processor(buildID, project string) composeapi.EventProcessor {
	return buildEventProcessor{
		registry: r,
		buildID:  buildID,
		project:  project,
	}
}

func (r *buildRegistry) writer(buildID, project, stream string) *buildStreamWriter {
	return &buildStreamWriter{
		registry: r,
		buildID:  buildID,
		project:  project,
		stream:   stream,
	}
}

func (r *buildRegistry) list() []buildSummary {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]buildSummary, 0, len(r.order))
	for i := len(r.order) - 1; i >= 0; i-- {
		record := r.builds[r.order[i]]
		if record == nil {
			continue
		}
		out = append(out, buildSummary{
			ID:         record.id,
			Project:    record.project,
			Status:     record.status,
			StartedAt:  record.startedAt,
			FinishedAt: record.finishedAt,
			Success:    record.success,
			StreamURL:  buildURL(record.id),
		})
	}
	return out
}

func (r *buildRegistry) stream(ctx context.Context, buildID string, w http.ResponseWriter) error {
	backlog, ch, unsubscribe, err := r.subscribe(buildID)
	if err != nil {
		return err
	}
	defer unsubscribe()

	stream, err := newTextSSEStream(w)
	if err != nil {
		return err
	}

	for _, msg := range backlog {
		if err := stream.Send("message", string(mustMarshalSSE(msg))); err != nil {
			return nil
		}
	}
	if ch == nil {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send("message", string(mustMarshalSSE(msg))); err != nil {
				return nil
			}
		}
	}
}

func (r *buildRegistry) subscribe(buildID string) ([]sseMessage, <-chan sseMessage, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	record := r.builds[buildID]
	if record == nil {
		return nil, nil, func() {}, errdefs.NotFound(fmt.Errorf("build %q not found", buildID))
	}

	backlog := append([]sseMessage(nil), record.messages...)
	if record.finishedAt != nil {
		return backlog, nil, func() {}, nil
	}

	id := record.nextSubID
	record.nextSubID++
	ch := make(chan sseMessage, 128)
	record.subscribers[id] = ch
	return backlog, ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		current := r.builds[buildID]
		if current == nil {
			return
		}
		subscriber, ok := current.subscribers[id]
		if !ok {
			return
		}
		delete(current.subscribers, id)
		close(subscriber)
	}, nil
}

func (r *buildRegistry) emit(buildID string, msg sseMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	record := r.builds[buildID]
	if record == nil {
		return
	}
	record.messages = append(record.messages, msg)
	for _, ch := range record.subscribers {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (p buildEventProcessor) Start(context.Context, string) {}

func (p buildEventProcessor) On(events ...composeapi.Resource) {
	for _, event := range events {
		message := renderBuildResource(event)
		if message == "" {
			continue
		}
		source := event.ID
		if source == "" {
			source = composeapi.ResourceCompose
		}
		p.registry.emit(p.buildID, sseMessage{
			Project: p.project,
			Stream:  "status",
			Source:  source,
			Message: message,
			Time:    time.Now().UTC(),
		})
	}
}

func (p buildEventProcessor) Done(string, bool) {}

func (w *buildStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending += strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\r", "\n")
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.registry.emit(w.buildID, sseMessage{
			Project: w.project,
			Stream:  w.stream,
			Source:  composeapi.ResourceCompose,
			Message: line,
			Time:    time.Now().UTC(),
		})
	}
	return len(p), nil
}

func renderBuildResource(event composeapi.Resource) string {
	parts := make([]string, 0, 3)
	if event.Text != "" {
		parts = append(parts, event.Text)
	}
	if event.Details != "" {
		parts = append(parts, event.Details)
	}
	if event.Percent > 0 {
		parts = append(parts, fmt.Sprintf("%d%%", event.Percent))
	}
	if len(parts) == 0 {
		if event.ID == "" {
			return ""
		}
		return event.ID
	}
	return strings.Join(parts, " ")
}

func mustMarshalSSE(msg sseMessage) []byte {
	payload, err := marshalSSE(msg)
	if err != nil {
		return []byte("{}")
	}
	return payload
}
