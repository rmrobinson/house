package bridge

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Pump relays every message sink produces to send, in order, until ctx is
// done (returns nil) or the sink is disconnected for falling behind
// (returns ErrStreamFellBehind) - the same subscribe/relay shape every
// Source-backed gRPC server stream in this codebase follows (see
// Facade.StreamUpdates, API.StreamUpdates, Service.StreamHouseUpdates), so
// each caller need only handle building and sending its own initial
// snapshot before calling Pump. If send returns an error, Pump stops and
// returns it unchanged.
//
// Pump panics if sink ever produces a message that isn't of type T - every
// Source is fed exactly one message type by its owner, so a mismatch is a
// caller bug, not a runtime condition worth recovering from.
func Pump[T proto.Message](ctx context.Context, sink *Sink, send func(T) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-sink.Messages():
			if !ok {
				return ErrStreamFellBehind
			}
			update, castOk := msg.(T)
			if !castOk {
				panic(fmt.Sprintf("bridge.Pump: received message of type %T, want %T", msg, update))
			}
			if err := send(update); err != nil {
				return err
			}
		}
	}
}
