package bridge

import (
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// sinkBufferSize is how many messages a sink can fall behind by before it's
// disconnected. It's sized for the largest burst a single event produces - a
// bridge reconnecting to a facade re-publishes one update per device it owns,
// all at once - rather than for steady-state traffic, which never gets close.
const sinkBufferSize = 1024

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

// NewSink creates a message sink for this source that receives every
// message broadcast to it.
func (s *Source) NewSink() *Sink {
	return s.NewFilteredSink(nil)
}

// NewFilteredSink creates a message sink that only receives messages for
// which filter returns true; filter may be nil, equivalent to NewSink. A
// message filter rejects is dropped before it ever reaches the sink's
// buffered channel, so a subscriber only interested in a subset of a
// source's messages (e.g. one building's worth, out of a source shared by
// every building) isn't at risk of its fixed-size buffer being crowded out,
// or disconnected (see SendMessage), by messages it was always going to
// discard.
func (s *Source) NewFilteredSink(filter func(proto.Message) bool) *Sink {
	sink := &Sink{
		id:      uuid.New().String(),
		channel: make(chan proto.Message, sinkBufferSize),
		filter:  filter,
		source:  s,
	}

	s.sinksLock.Lock()
	s.sinks[sink.id] = sink
	s.sinksLock.Unlock()

	s.logger.Debug("added watcher",
		zap.String("channel_id", sink.id))
	return sink
}

// SendMessage sends a message to every created sink whose filter (if any)
// accepts it, without blocking on any of them. A sink whose buffer is full
// is disconnected (its Messages channel closed) rather than having the
// message silently dropped: every consumer here is a StreamUpdates-style
// stream whose state is only correct if it sees every update it didn't
// filter out, so a gap would leave it permanently stale. Closing the
// channel ends that stream instead, and the client's reconnect gets a fresh
// initial snapshot.
func (s *Source) SendMessage(msg proto.Message) {
	s.sinksLock.Lock()
	defer s.sinksLock.Unlock()

	for id, sink := range s.sinks {
		if sink.filter != nil && !sink.filter(msg) {
			continue
		}
		select {
		case sink.channel <- msg:
		default:
			s.logger.Warn("watcher fell behind, disconnecting it",
				zap.String("channel_id", id),
			)
			delete(s.sinks, id)
			sink.closeChannel()
		}
	}
}

func (s *Source) removeSink(sink *Sink) {
	s.sinksLock.Lock()
	delete(s.sinks, sink.id)
	s.sinksLock.Unlock()
}
