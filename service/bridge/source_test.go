package bridge

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
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
