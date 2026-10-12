package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rmrobinson/house/api/command"
)

// CommandClass groups commands by what they change about a device, which is what decides whether
// one command makes another obsolete.
type CommandClass string

const (
	// ClassPower is turning a device on or off.
	ClassPower CommandClass = "power"
	// ClassLevel is setting a brightness, absolutely or relatively.
	ClassLevel CommandClass = "level"
	// ClassColour is setting a colour or colour temperature.
	ClassColour CommandClass = "colour"
)

// ClassOf returns cmd's class. The three classes above cover a light; every other command is its
// own class, named after its Command.details field. Those are never superseded (see Supersedes),
// but are kept distinct so one kind of command never queues behind, or cancels, another's rules.
func ClassOf(cmd *command.Command) CommandClass {
	m := cmd.ProtoReflect()
	oneof := m.Descriptor().Oneofs().ByName("details")
	if oneof == nil {
		return "unknown"
	}
	fd := m.WhichOneof(oneof)
	if fd == nil {
		return "unknown"
	}
	switch name := string(fd.Name()); name {
	case "on_off":
		return ClassPower
	case "brightness_absolute", "brightness_relative":
		return ClassLevel
	case "colour":
		return ClassColour
	default:
		return CommandClass(name)
	}
}

// Supersedes reports whether a newer command of class newer makes an older one of class older
// obsolete: a command replaces any older command of its own class, and turning a device on or off
// is the final word over any brightness or colour change still pending.
//
// Only commands that WHOLLY replace the previous one can be superseded: on/off, brightness and
// colour each set a single value, so the newest is all that matters. Everything else is left
// alone, even against its own kind. A Toggle, for instance, changes only the settings it names
// (api/command/toggle.proto), so setting a purifier's child lock and then its LED are two
// independent commands - dropping the first because the second arrived would silently lose it.
// Such commands are still serialised and still bounded by MaxWait; they are just never cancelled.
//
// The on/off rule is the one that matters most. A brightness or colour command reaching a bulb
// after an Off turns it back on (zigbee2mqtt sets state ON along with a brightness), so a stale
// dim that finally gets through a slow link after the user pressed Off leaves the light on.
func Supersedes(newer, older CommandClass) bool {
	if !replaceable(older) {
		return false
	}
	if newer == older {
		return true
	}
	return newer == ClassPower && (older == ClassLevel || older == ClassColour)
}

// replaceable reports whether a command of class c sets one value outright, so a newer command
// can stand in for it. See Supersedes.
func replaceable(c CommandClass) bool {
	return c == ClassPower || c == ClassLevel || c == ClassColour
}

// DefaultCommandQueueWait is how long a command waits for its turn at a busy device before it is
// dropped as stale, when a CommandLane doesn't set MaxWait.
//
// It must exceed the longest a single command can legitimately hold the device - a bridge's write
// timeout (bridges/zigbee waits up to 8s for a device to confirm) - or a command queued behind one
// that is merely slow to time out would ALWAYS be dropped, however recent. At 10s a command
// survives waiting out one such command, but not two in a row: past that the user has moved on.
const DefaultCommandQueueWait = 10 * time.Second

// CommandLane orders the commands sent to ONE device, for a bridge whose device can only do one
// thing at a time and may be slow to confirm. Its zero value is ready to use.
//
// Without it, a device that stops acknowledging (a bulb at the edge of radio range, say) makes
// every caller queue for it, and each queued command is then sent in turn - long after the person
// who asked has moved on. A burst of presses becomes a train of stale commands delivered after the
// press that was supposed to have the last word. A lane instead guarantees:
//
//   - one command at a time reaches the device;
//   - a newer command supersedes older ones that are still waiting AND the one in flight (see
//     Supersedes): the older caller gets ErrCommandSuperseded at once, so the slot frees and the
//     newer command goes out immediately instead of after a timeout;
//   - a command that waits longer than MaxWait for its turn is dropped with ErrCommandStale, never
//     delivered late;
//   - a caller whose context ends while waiting is dropped without ever being sent.
//
// A command already on the wire can't be recalled, so a superseded command may still take effect;
// the guarantee is only that nothing older is sent after something newer.
type CommandLane struct {
	// MaxWait overrides DefaultCommandQueueWait if positive.
	MaxWait time.Duration

	once sync.Once
	slot chan struct{}

	mu      sync.Mutex
	entries []*laneEntry // in flight and waiting, in arrival order
}

type laneEntry struct {
	class  CommandClass
	cancel context.CancelCauseFunc
}

// Acquire waits for cmd's turn at the device. On success it returns a context to execute the
// command with - cancelled if a newer command supersedes this one mid-flight, so execution should
// stop waiting for the device when it ends - and a release func that must be called when the
// command is done. On failure it returns ErrCommandSuperseded, ErrCommandStale, or ctx's error,
// and nothing is held.
func (l *CommandLane) Acquire(ctx context.Context, cmd *command.Command) (context.Context, func(), error) {
	l.once.Do(func() { l.slot = make(chan struct{}, 1) })

	class := ClassOf(cmd)
	ectx, cancel := context.WithCancelCause(ctx)
	e := &laneEntry{class: class, cancel: cancel}

	l.mu.Lock()
	for _, old := range l.entries {
		if Supersedes(class, old.class) {
			old.cancel(ErrCommandSuperseded)
		}
	}
	l.entries = append(l.entries, e)
	l.mu.Unlock()

	wait := l.MaxWait
	if wait <= 0 {
		wait = DefaultCommandQueueWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case l.slot <- struct{}{}:
	case <-ectx.Done():
		l.drop(e)
		return nil, nil, SupersededError(ectx, ectx.Err())
	case <-timer.C:
		l.drop(e)
		return nil, nil, ErrCommandStale
	}

	// select picks at random among ready cases, so a command superseded or cancelled in the same
	// instant it won the slot must still be turned away here.
	if err := ectx.Err(); err != nil {
		<-l.slot
		l.drop(e)
		return nil, nil, SupersededError(ectx, err)
	}

	// release is safe to call more than once (callers defer it): a second call must neither block
	// on an empty slot nor free one that another command has since taken.
	var once sync.Once
	return ectx, func() {
		once.Do(func() {
			l.drop(e)
			<-l.slot
		})
	}, nil
}

func (l *CommandLane) drop(e *laneEntry) {
	l.mu.Lock()
	for i, x := range l.entries {
		if x == e {
			l.entries = append(l.entries[:i], l.entries[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
	e.cancel(nil)
}

// SupersededError returns ErrCommandSuperseded if ctx (a context from CommandLane.Acquire) was
// cancelled because a newer command replaced its command, and err otherwise. A bridge applies it
// to whatever its command execution returned, since a command abandoned mid-flight surfaces there
// as a bare context error.
func SupersededError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrCommandSuperseded) {
		return ErrCommandSuperseded
	}
	return err
}
