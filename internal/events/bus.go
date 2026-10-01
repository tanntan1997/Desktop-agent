// Package events is a tiny in-process pub/sub used to fan device and job
// notifications out to the tunnel and to local SSE subscribers.
package events

import "../../../mobile-device-agent/internal/events/sync"

// Event is a named notification with a JSON-serialisable payload.
type Event struct {
	Name string `json:"event"`
	Data any    `json:"data"`
}

// Bus delivers events to subscribers without ever blocking publishers; a
// subscriber that falls behind its buffer drops events.
type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func NewBus() *Bus { return &Bus{subs: map[int]chan Event{}} }

// Subscribe returns a channel of events and a function that unsubscribes.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}

func (b *Bus) Publish(name string, data any) {
	ev := Event{Name: name, Data: data}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
