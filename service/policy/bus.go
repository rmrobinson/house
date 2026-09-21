package policy

import "sync"

// Event is a single message published on a Bus topic.
type Event struct {
	Topic   string
	Payload any
}

// Bus is a minimal in-memory topic-based pub/sub used to drive EventCondition
// and to let the engine fan device/house state changes out to conditions
// without every condition polling the state cache directly.
//
// It is scoped to a single Engine instance; it is not a substitute for a
// cross-service event bus.
type Bus struct {
	mu   sync.Mutex
	subs map[string][]chan Event
}

// NewBus creates an empty Bus.
func NewBus() *Bus {
	return &Bus{
		subs: make(map[string][]chan Event),
	}
}

// Subscribe returns a channel that receives every Event published on topic
// until Unsubscribe is called with the same topic and channel.
func (b *Bus) Subscribe(topic string) <-chan Event {
	ch := make(chan Event, 16)

	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()

	return ch
}

// Unsubscribe stops delivery to ch. It does not close ch: a concurrent
// Publish may already be about to send on it, and closing would race with
// that send. Callers must stop reading ch some other way (e.g. their own
// ctx), not by watching for it to close.
//
// It is a no-op if ch is not currently subscribed to topic.
func (b *Bus) Unsubscribe(topic string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs := b.subs[topic]
	for i, c := range subs {
		if c == ch {
			b.subs[topic] = append(subs[:i], subs[i+1:]...)
			return
		}
	}
}

// Publish delivers ev to every current subscriber of ev.Topic. Delivery is
// non-blocking: a subscriber whose channel is full misses the event rather
// than stalling the publisher.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	subs := append([]chan Event(nil), b.subs[ev.Topic]...)
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
