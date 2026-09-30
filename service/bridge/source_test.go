package bridge

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type testMessage struct {
	value string
}

func (tm *testMessage) Reset()                             {}
func (tm *testMessage) ProtoMessage()                      {}
func (tm *testMessage) ProtoReflect() protoreflect.Message { return nil }

func TestNewSink(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))

	var wg sync.WaitGroup

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(t *testing.T, wg *sync.WaitGroup) {
			sink := s.NewSink()
			assert.NotNil(t, sink)
			wg.Done()
		}(t, &wg)
	}

	wg.Wait()
	assert.Equal(t, 1000, len(s.sinks))
}

func TestSinkRemove(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))

	var wg sync.WaitGroup

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(t *testing.T, wg *sync.WaitGroup) {
			sink := s.NewSink()
			assert.NotNil(t, sink)
			sink.Close()

			wg.Done()
		}(t, &wg)
	}

	wg.Wait()
	assert.Equal(t, 0, len(s.sinks))
}

func TestMessaging(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))

	var sendMessageWg sync.WaitGroup
	var messageReceivedWg sync.WaitGroup

	testMsg := &testMessage{"asdf123"}

	for i := 0; i < 1000; i++ {
		sendMessageWg.Add(1)
		messageReceivedWg.Add(1)
		go func(t *testing.T) {
			sink := s.NewSink()
			assert.NotNil(t, sink)
			sendMessageWg.Done()

			max := rand.Intn(5)

			for i := 0; i < max; i++ {
				msg := <-sink.Messages()
				assert.Equal(t, testMsg, msg)
			}

			sink.Close()
			messageReceivedWg.Done()
		}(t)
	}

	sendMessageWg.Wait()

	for i := 0; i < 5; i++ {
		s.SendMessage(testMsg)
	}

	messageReceivedWg.Wait()
	assert.Equal(t, 0, len(s.sinks))
}

func TestSlowSinkIsDisconnectedNotSilentlyDropped(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	slow := s.NewSink()
	defer slow.Close()

	testMsg := &testMessage{"asdf123"}
	for i := 0; i < sinkBufferSize+1; i++ {
		s.SendMessage(testMsg)
	}

	// Everything buffered before the overflow is still delivered, then the
	// channel is closed rather than the overflowing message vanishing.
	for i := 0; i < sinkBufferSize; i++ {
		msg, ok := <-slow.Messages()
		assert.True(t, ok)
		assert.Equal(t, testMsg, msg)
	}
	_, ok := <-slow.Messages()
	assert.False(t, ok)
	assert.Equal(t, 0, len(s.sinks))
}

// TestFilteredSink_RejectedMessagesNeverTakeABufferSlot is the regression
// case for the multi-building StreamHouseUpdates buffer-starvation bug: a
// filtered sink must never enqueue a message its filter rejects, so a flood
// of messages the subscriber doesn't want can't crowd out the fixed-size
// buffer ahead of messages it does want - and can't trigger the
// fell-behind disconnect in SendMessage either, since it never counts
// against the buffer.
func TestFilteredSink_RejectedMessagesNeverTakeABufferSlot(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))

	wanted := &testMessage{"wanted"}
	unwanted := &testMessage{"unwanted"}

	sink := s.NewFilteredSink(func(msg proto.Message) bool {
		return msg.(*testMessage).value == "wanted"
	})
	defer sink.Close()

	// Flood well past the sink's fixed buffer size with messages the filter
	// rejects - none of them should ever occupy a buffer slot, let alone
	// overflow it.
	for i := 0; i < sinkBufferSize*2; i++ {
		s.SendMessage(unwanted)
	}
	s.SendMessage(wanted)

	select {
	case msg, ok := <-sink.Messages():
		require.True(t, ok, "sink must not have been disconnected by rejected messages")
		assert.Equal(t, wanted, msg, "the wanted message must not have been crowded out by rejected ones")
	default:
		t.Fatal("expected the wanted message to be buffered")
	}

	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected extra message: %+v", msg)
	default:
	}
}

// TestNewSink_HasNoFilter documents that NewSink is exactly
// NewFilteredSink(nil) - every message reaches it, same as before
// NewFilteredSink existed.
func TestNewSink_HasNoFilter(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()
	assert.Nil(t, sink.filter)
}
