package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// otherTestMessage is a second dummy proto.Message, distinct from
// testMessage (defined in source_test.go), so TestPump_PanicsOnUnexpectedMessageType
// can produce a genuine type mismatch for Pump's type-assert guard.
type otherTestMessage struct{}

func (om *otherTestMessage) Reset()                             {}
func (om *otherTestMessage) ProtoMessage()                      {}
func (om *otherTestMessage) ProtoReflect() protoreflect.Message { return nil }

func TestPump_RelaysMessagesInOrder(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()

	s.SendMessage(&testMessage{"one"})
	s.SendMessage(&testMessage{"two"})

	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- Pump(ctx, sink, func(m *testMessage) error {
			got = append(got, m.value)
			if len(got) == 2 {
				cancel()
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Pump did not return after context cancellation")
	}
	assert.Equal(t, []string{"one", "two"}, got)
}

func TestPump_ContextDoneReturnsNil(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Pump(ctx, sink, func(m *testMessage) error {
		t.Fatal("send should never be called - no messages were sent")
		return nil
	})
	assert.NoError(t, err)
}

func TestPump_FellBehindReturnsErrStreamFellBehind(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()

	for i := 0; i < sinkBufferSize+1; i++ {
		s.SendMessage(&testMessage{"flood"})
	}

	err := Pump(context.Background(), sink, func(m *testMessage) error {
		return nil
	})
	assert.ErrorIs(t, err, ErrStreamFellBehind)
}

func TestPump_SendErrorStopsAndPropagates(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()

	s.SendMessage(&testMessage{"one"})
	s.SendMessage(&testMessage{"two"})

	wantErr := errors.New("boom")
	calls := 0
	err := Pump(context.Background(), sink, func(m *testMessage) error {
		calls++
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, calls, "Pump must stop relaying after send returns an error")
}

func TestPump_PanicsOnUnexpectedMessageType(t *testing.T) {
	s := NewSource(zaptest.NewLogger(t))
	sink := s.NewSink()
	defer sink.Close()

	s.SendMessage(&testMessage{"wrong-type-for-this-pump"})

	assert.Panics(t, func() {
		_ = Pump(context.Background(), sink, func(m *otherTestMessage) error {
			return nil
		})
	})
}
