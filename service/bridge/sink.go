package bridge

import (
	"google.golang.org/protobuf/proto"
)

// Sink is an implementation of a message sync; it receives messages broadcast by its parent source.
type Sink struct {
	id      string
	channel chan proto.Message

	source *Source
}

// Messages returns the read channel of messages broadcast by the source.
// The backing channel is buffered to allow for additional messages to be generated
// while the current message is being processed; that being said the sink has a responsibility
// to consume messages from this channel as quickly as possible.
func (s *Sink) Messages() <-chan proto.Message {
	return s.channel
}

// Close releases any resources allocated as part of this sink's creation.
func (s *Sink) Close() {
	s.source.removeSink(s)
	close(s.channel)
}
