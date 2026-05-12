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
