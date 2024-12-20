package bridge

import (
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Source represents a message source that will be broadcast to its sinks.
type Source struct {
	logger *zap.Logger

	sinks     map[string]*Sink
	sinksLock sync.Mutex
}

// NewSource creates a new message source.
func NewSource(logger *zap.Logger) *Source {
	return &Source{
		logger: logger,
		sinks:  map[string]*Sink{},
	}
}

// NewSink creates a message sink for this source.
func (s *Source) NewSink() *Sink {
	sink := &Sink{
		id:      uuid.New().String(),
		channel: make(chan proto.Message, 10),
		source:  s,
	}

	s.sinksLock.Lock()
	s.sinks[sink.id] = sink
	s.sinksLock.Unlock()

	s.logger.Debug("added watcher",
		zap.String("channel_id", sink.id))
	return sink
}

// SendMessage sends a message to all created sinks.
func (s *Source) SendMessage(msg proto.Message) {
	s.sinksLock.Lock()

	for _, sink := range s.sinks {
		// Try to write the message to the sink or log that the write failed
		select {
		case sink.channel <- msg:
			// Add logging here if needed
		default:
			s.logger.Debug("channel blocked",
				zap.String("channel_id", sink.id),
			)
		}
	}

	s.sinksLock.Unlock()
}

func (s *Source) removeSink(sink *Sink) {
	s.sinksLock.Lock()
	delete(s.sinks, sink.id)
	s.sinksLock.Unlock()
}
