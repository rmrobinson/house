package bridge

import (
	"sync"

	"google.golang.org/protobuf/proto"
)

// Sink is an implementation of a message sync; it receives messages broadcast by its parent source.
type Sink struct {
	id      string
	channel chan proto.Message
	// filter, if set, is consulted before a message is written to channel -
	// a message it rejects is dropped without ever taking a buffer slot.
	// See Source.NewFilteredSink.
	filter func(proto.Message) bool

	source    *Source
	closeOnce sync.Once
}

// Messages returns the read channel of messages broadcast by the source.
// The backing channel is buffered to allow for additional messages to be generated
// while the current message is being processed; that being said the sink has a responsibility
// to consume messages from this channel as quickly as possible. The channel is
// closed if the sink falls too far behind (see Source.SendMessage), and the
// consumer must then stop and let its client resync.
func (s *Sink) Messages() <-chan proto.Message {
	return s.channel
}

// Close releases any resources allocated as part of this sink's creation.
// Safe to call after the source has already disconnected this sink.
func (s *Sink) Close() {
	s.source.removeSink(s)
	s.closeChannel()
}

func (s *Sink) closeChannel() {
	s.closeOnce.Do(func() { close(s.channel) })
}
