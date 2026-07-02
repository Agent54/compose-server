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
	"sync"
	"time"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

type watchRegistry struct {
	mu        sync.Mutex
	resources map[string]*watchResource
	mirror    func(sseMessage)
}

type watchResource struct {
	project     string
	cancel      context.CancelFunc
	subscribers map[int]chan sseMessage
	nextID      int
}

type watchConsumer struct {
	project string
	send    func(sseMessage)
}

func newWatchRegistry() *watchRegistry {
	return &watchRegistry{resources: map[string]*watchResource{}}
}

func (r *watchRegistry) setMirror(mirror func(sseMessage)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mirror = mirror
}

func (r *watchRegistry) start(project string, run func(context.Context, composeapi.LogConsumer) error) error {
	r.mu.Lock()
	prev := r.resources[project]
	ctx, cancel := context.WithCancel(context.Background())
	resource := &watchResource{
		project:     project,
		cancel:      cancel,
		subscribers: map[int]chan sseMessage{},
	}
	r.resources[project] = resource
	r.mu.Unlock()

	if prev != nil {
		prev.closeSubscribers()
		prev.cancel()
	}

	consumer := watchConsumer{
		project: project,
		send: func(msg sseMessage) {
			r.broadcast(project, msg)
		},
	}
	r.broadcast(project, sseMessage{Project: project, Stream: "status", Source: composeapi.WatchLogger, Message: "Watch enabled", Time: time.Now().UTC()})
	go func() {
		err := run(ctx, consumer)
		if err != nil && ctx.Err() == nil {
			r.broadcast(project, sseMessage{Project: project, Stream: "stderr", Source: composeapi.WatchLogger, Message: err.Error(), Time: time.Now().UTC()})
		}
		r.broadcast(project, sseMessage{Project: project, Stream: "status", Source: composeapi.WatchLogger, Message: "Watch disabled", Time: time.Now().UTC()})
		r.mu.Lock()
		current := r.resources[project]
		if current == resource {
			delete(r.resources, project)
			resource.closeSubscribers()
		}
		r.mu.Unlock()
	}()
	return nil
}

func (r *watchRegistry) stop(project string) bool {
	r.mu.Lock()
	resource := r.resources[project]
	if resource != nil {
		delete(r.resources, project)
	}
	r.mu.Unlock()
	if resource == nil {
		return false
	}
	resource.closeSubscribers()
	resource.cancel()
	return true
}

func (r *watchRegistry) stopAll() int {
	r.mu.Lock()
	resources := make([]*watchResource, 0, len(r.resources))
	for name, resource := range r.resources {
		delete(r.resources, name)
		resources = append(resources, resource)
	}
	r.mu.Unlock()

	for _, resource := range resources {
		resource.closeSubscribers()
		resource.cancel()
	}
	return len(resources)
}

func (r *watchRegistry) snapshot() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]struct{}, len(r.resources))
	for name := range r.resources {
		out[name] = struct{}{}
	}
	return out
}

func (r *watchRegistry) stream(ctx context.Context, project string, w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}

	ch, unsubscribe := r.subscribe(project)
	if ch == nil {
		return errWatchNotFound(project)
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			payload, err := marshalSSE(msg)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func (r *watchRegistry) subscribe(project string) (<-chan sseMessage, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resource := r.resources[project]
	if resource == nil {
		return nil, func() {}
	}
	id := resource.nextID
	resource.nextID++
	ch := make(chan sseMessage, 128)
	resource.subscribers[id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if current := r.resources[project]; current != nil {
			if subscriber, ok := current.subscribers[id]; ok {
				delete(current.subscribers, id)
				close(subscriber)
			}
		}
	}
}

func (r *watchRegistry) broadcast(project string, msg sseMessage) {
	r.mu.Lock()
	resource := r.resources[project]
	mirror := r.mirror
	var subscribers []chan sseMessage
	if resource != nil {
		subscribers = make([]chan sseMessage, 0, len(resource.subscribers))
		for _, ch := range resource.subscribers {
			subscribers = append(subscribers, ch)
		}
	}
	r.mu.Unlock()

	if resource == nil {
		if mirror != nil {
			mirror(msg)
		}
		return
	}
	for _, ch := range subscribers {
		select {
		case ch <- msg:
		default:
		}
	}
	if mirror != nil {
		mirror(msg)
	}
}

func (r *watchResource) closeSubscribers() {
	for id, ch := range r.subscribers {
		delete(r.subscribers, id)
		close(ch)
	}
}

func (c watchConsumer) Log(containerName, message string) {
	c.send(sseMessage{Project: c.project, Stream: "stdout", Source: containerName, Message: message, Time: time.Now().UTC()})
}

func (c watchConsumer) Err(containerName, message string) {
	c.send(sseMessage{Project: c.project, Stream: "stderr", Source: containerName, Message: message, Time: time.Now().UTC()})
}

func (c watchConsumer) Status(containerName, message string) {
	c.send(sseMessage{Project: c.project, Stream: "status", Source: containerName, Message: message, Time: time.Now().UTC()})
}
