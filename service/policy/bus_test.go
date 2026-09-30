package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBusPublishSubscribe(t *testing.T) {
	bus := NewBus()
	ch := bus.Subscribe("topic.a")

	bus.Publish(Event{Topic: "topic.a", Payload: "hello"})
	bus.Publish(Event{Topic: "topic.b", Payload: "ignored"})

	select {
	case ev := <-ch:
		assert.Equal(t, "topic.a", ev.Topic)
		assert.Equal(t, "hello", ev.Payload)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	select {
	case ev, ok := <-ch:
		t.Fatalf("unexpected second event: %+v (ok=%v)", ev, ok)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	ch := bus.Subscribe("topic.a")

	bus.Unsubscribe("topic.a", ch)
	bus.Publish(Event{Topic: "topic.a", Payload: "noop"})

	select {
	case ev, ok := <-ch:
		t.Fatalf("unexpected delivery after Unsubscribe: %+v (ok=%v)", ev, ok)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBusMultipleSubscribers(t *testing.T) {
	bus := NewBus()
	ch1 := bus.Subscribe("topic.a")
	ch2 := bus.Subscribe("topic.a")

	bus.Publish(Event{Topic: "topic.a", Payload: 1})

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			assert.Equal(t, 1, ev.Payload)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}
