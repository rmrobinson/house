package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

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

// detailsField returns which Command.details field cmd sets, or "" if none.
func detailsField(cmd *command.Command) string {
	m := cmd.ProtoReflect()
	oneof := m.Descriptor().Oneofs().ByName("details")
	if oneof == nil {
		return ""
	}
	fd := m.WhichOneof(oneof)
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

// ClassOf returns cmd's class. The three classes above cover a light; every other command is its
// own class, named after its Command.details field. Those are never superseded (see Supersedes),
// but are kept distinct so one kind of command never queues behind, or cancels, another's rules.
func ClassOf(cmd *command.Command) CommandClass {
	switch name := detailsField(cmd); name {
	case "":
		return "unknown"
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

// idempotent reports whether sending cmd twice is the same as sending it once: it sets an absolute
// value (on/off, an absolute brightness, a colour) rather than stepping from the current one. A
// relative brightness is NOT idempotent, so it is never treated as a repeat of an earlier one.
func idempotent(cmd *command.Command) bool {
	switch detailsField(cmd) {
	case "on_off", "brightness_absolute", "colour":
		return true
	}
	return false
}

// sameEffect reports whether a and b are both idempotent commands asking for exactly the same
// thing, so that sending b after a would change nothing a had not already asked for.
func sameEffect(a, b *command.Command) bool {
	if !idempotent(a) || !idempotent(b) || detailsField(a) != detailsField(b) {
		return false
	}
	ma, mb := a.ProtoReflect(), b.ProtoReflect()
	oneof := ma.Descriptor().Oneofs().ByName("details")
	fa, fb := ma.WhichOneof(oneof), mb.WhichOneof(oneof)
	return proto.Equal(ma.Get(fa).Message().Interface(), mb.Get(fb).Message().Interface())
}

// DefaultCommandQueueWait is how long a command waits for its turn at a busy device before it is
// dropped as stale, when a CommandLane doesn't set MaxWait.
//
// It must exceed the longest a single command can legitimately hold the device - a bridge's write
// timeout (bridges/zigbee waits up to 8s for a device to confirm) - or a command queued behind one
// that is merely slow to time out would ALWAYS be dropped, however recent. At 10s a command
// survives waiting out one such command, but not two in a row: past that the user has moved on.
const DefaultCommandQueueWait = 10 * time.Second

// ErrOwnerGaveUp is what Flight.Wait returns when the command it joined ended because ITS caller
// gave up (the caller's deadline passed or its request was cancelled) - not because of anything
// the device did. The joiner's own request is still alive and nothing was sent on its behalf, so it
// should Acquire again and become the sender itself, rather than fail with someone else's
// cancellation.
var ErrOwnerGaveUp = errors.New("the command joined was abandoned by its own caller")

// joinedOutcome is the outcome a joiner sees for a command that ended with err: the same error,
// except that an end caused by the OWNER's context is reported as ErrOwnerGaveUp. Superseded,
// stale and device errors are gRPC status errors, which never wrap a context error, so they pass
// through unchanged.
func joinedOutcome(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrOwnerGaveUp
	}
	return err
}

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
//   - a command identical in effect to one already pending is not sent again: its caller joins the
//     pending command and gets the same outcome (see Acquire). A slow device is already behind;
//     repeating what it hasn't yet done only lengthens its queue, and gives an old copy another
//     chance to land after a newer command;
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
	cmd    *command.Command
	class  CommandClass
	ctx    context.Context
	cancel context.CancelCauseFunc

	// done is closed, with err set, when the command's outcome is known - however it ends: it
	// completed, failed, was superseded, expired, or its caller gave up. Callers that joined it
	// wait on done.
	done     chan struct{}
	err      error
	resolver sync.Once
}

func (e *laneEntry) resolve(err error) {
	e.resolver.Do(func() {
		e.err = joinedOutcome(err)
		close(e.done)
	})
}

// Lease is the right to send one command to the device. Hold it until the command is done, then
// call Release.
type Lease struct {
	ctx     context.Context
	release func(err error)
}

// Context is the context to execute the command with. It is cancelled if a newer command
// supersedes this one while it is in flight, so execution should stop waiting for the device when
// it ends (and report the outcome through SupersededError).
func (ls *Lease) Context() context.Context { return ls.ctx }

// Release ends the lease, passing on the command's outcome (nil for success) to any callers that
// joined it, and frees the device for the next command. It is safe to call more than once: only
// the first call counts, so a deferred Release can't free a slot another command has since taken.
func (ls *Lease) Release(err error) { ls.release(err) }

// Flight is a command identical to the caller's that is already pending. The caller doesn't send
// anything; it waits for that command's outcome.
type Flight struct{ e *laneEntry }

// Wait blocks until the command joined has an outcome and returns it (nil if it succeeded), or
// returns ctx's error if ctx ends first. Giving up waiting doesn't affect the command joined.
// ErrOwnerGaveUp means the command joined was abandoned by its own caller: call Acquire again.
//
// A joiner still owns the bookkeeping for its OWN request - for an asynchronous command, completing
// the command id it was given - since the command joined carries the owner's id, not the joiner's.
func (f *Flight) Wait(ctx context.Context) error {
	select {
	case <-f.e.done:
		return f.e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Acquire waits for cmd's turn at the device, and returns exactly one of:
//
//   - a Lease, when the caller should send cmd;
//   - a Flight, when an identical-in-effect command is already pending and nothing needs sending:
//     the caller waits on it instead. Only idempotent commands join (see idempotent), and only
//     the newest pending command that would conflict with this one is a candidate - if that is
//     something different (an Off, then another On), this command supersedes it as usual rather
//     than joining an older, equal one behind it;
//   - an error - ErrCommandSuperseded, ErrCommandStale, or ctx's - with nothing held.
func (l *CommandLane) Acquire(ctx context.Context, cmd *command.Command) (*Lease, *Flight, error) {
	l.once.Do(func() { l.slot = make(chan struct{}, 1) })

	class := ClassOf(cmd)

	l.mu.Lock()
	if idempotent(cmd) {
		// The newest live entry this command would conflict with decides: join it only if it is
		// the same request.
		for i := len(l.entries) - 1; i >= 0; i-- {
			old := l.entries[i]
			if old.ctx.Err() != nil || !Supersedes(class, old.class) {
				continue
			}
			if sameEffect(cmd, old.cmd) {
				l.mu.Unlock()
				return nil, &Flight{e: old}, nil
			}
			break
		}
	}

	ectx, cancel := context.WithCancelCause(ctx)
	e := &laneEntry{cmd: cmd, class: class, ctx: ectx, cancel: cancel, done: make(chan struct{})}
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
		err := SupersededError(ectx, ectx.Err())
		l.drop(e, err)
		return nil, nil, err
	case <-timer.C:
		l.drop(e, ErrCommandStale)
		return nil, nil, ErrCommandStale
	}

	// select picks at random among ready cases, so a command superseded or cancelled in the same
	// instant it won the slot must still be turned away here.
	if err := ectx.Err(); err != nil {
		err = SupersededError(ectx, err)
		<-l.slot
		l.drop(e, err)
		return nil, nil, err
	}

	var once sync.Once
	return &Lease{ctx: ectx, release: func(err error) {
		once.Do(func() {
			l.drop(e, err)
			<-l.slot
		})
	}}, nil, nil
}

// drop removes e from the lane and records its outcome for anyone who joined it.
func (l *CommandLane) drop(e *laneEntry, err error) {
	l.mu.Lock()
	for i, x := range l.entries {
		if x == e {
			l.entries = append(l.entries[:i], l.entries[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
	e.resolve(err)
	e.cancel(nil)
}

// SupersededError returns ErrCommandSuperseded if ctx (a Lease's context) was cancelled because a
// newer command replaced its command, and err otherwise. A bridge applies it to whatever its
// command execution returned, since a command abandoned mid-flight surfaces there as a bare
// context error.
func SupersededError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrCommandSuperseded) {
		return ErrCommandSuperseded
	}
	return err
}
