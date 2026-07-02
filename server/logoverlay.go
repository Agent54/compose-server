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

import "sync"

type logRegistry struct {
	mu          sync.Mutex
	subscribers map[string]map[int]chan sseMessage
	nextID      int
}

func newLogRegistry() *logRegistry {
	return &logRegistry{subscribers: map[string]map[int]chan sseMessage{}}
}

func (r *logRegistry) subscribe(project string) (<-chan sseMessage, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := r.nextID
	if r.subscribers[project] == nil {
		r.subscribers[project] = map[int]chan sseMessage{}
	}
	ch := make(chan sseMessage, 128)
	r.subscribers[project][id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subscribers := r.subscribers[project]; subscribers != nil {
			if subscriber := subscribers[id]; subscriber != nil {
				delete(subscribers, id)
				close(subscriber)
			}
			if len(subscribers) == 0 {
				delete(r.subscribers, project)
			}
		}
	}
}

func (r *logRegistry) broadcast(project string, msg sseMessage) {
	r.mu.Lock()
	subscribers := make([]chan sseMessage, 0, len(r.subscribers[project]))
	for _, ch := range r.subscribers[project] {
		subscribers = append(subscribers, ch)
	}
	r.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- msg:
		default:
		}
	}
}
