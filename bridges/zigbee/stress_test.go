package main

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/command"
)

// A randomized soak of the whole command path for ONE lamp: many callers issuing mixed commands
// (on, off, absolute and relative brightness) with some deadlines tight enough to expire in the
// queue, a device whose reports arrive out of step with the commands, and a broker that fails and
// recovers part-way. It asserts only what must always hold - every caller gets an answer, nothing
// deadlocks or races (run it under -race), and afterwards the lamp still takes a command
// promptly, i.e. no slot or queue entry was leaked by any of the paths above (superseded, joined,
// stale, timed out, rolled back).
func TestCommandPathStaysLiveUnderConcurrentLoad(t *testing.T) {
	shrinkWriteTimeout(t, 15*time.Millisecond)
	nc, fc := newTestNetworkConn(t, zigbeeConfig{})
	nc.buildDevice(lightDevice(true, false, false))
	nc.queryWG.Wait()
	t.Cleanup(nc.mqtt.quiesce) // never leave a report handler logging after the test is over

	commands := []func() *command.Command{
		func() *command.Command { return onCommand("zigbee-bulb") },
		func() *command.Command { return offCommand("zigbee-bulb") },
		func() *command.Command { return levelCommand("zigbee-bulb", 30) },
		func() *command.Command { return levelCommand("zigbee-bulb", 80) },
		func() *command.Command { return relativeCommand("zigbee-bulb", 10) },
		func() *command.Command { return relativeCommand("zigbee-bulb", -10) },
	}
	reports := []string{
		`{"state":"ON","brightness":254}`, `{"state":"OFF","brightness":127}`,
		`{"state":"ON","brightness":76}`, `{"state":"ON","brightness":203}`,
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup

	// The device's own reports, unrelated to what is being asked of it. Dispatched directly: the
	// fake client's deliver waits for the handlers to drain, which is not safe to do concurrently.
	bg.Add(1)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewSource(7))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(1+r.Intn(4)) * time.Millisecond):
				nc.mqtt.dispatch("zigbee2mqtt/lamp1", []byte(reports[r.Intn(len(reports))]))
			}
		}
	}()

	// The broker rejecting publishes on and off.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(7 * time.Millisecond):
				if i%3 == 0 {
					fc.setPublishErr(assert.AnError)
				} else {
					fc.setPublishErr(nil)
				}
			}
		}
	}()

	const callers = 120
	var answered atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(1000 + i)))
			time.Sleep(time.Duration(r.Intn(40)) * time.Millisecond)

			ctx := context.Background()
			if r.Intn(3) == 0 { // a deadline that can expire while queued behind a slow command
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(2+r.Intn(20))*time.Millisecond)
				defer cancel()
			}
			_, _ = nc.applyCommand(ctx, commands[r.Intn(len(commands))]())
			answered.Add(1)
		}(i)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatalf("deadlock: only %d of %d callers got an answer", answered.Load(), callers)
	}
	close(stop)
	bg.Wait()
	nc.mqtt.quiesce() // the queued device reports must finish being applied before the test ends

	assert.Equal(t, int64(callers), answered.Load())

	// The lamp must still work: nothing above may have leaked its command slot or a queue entry.
	fc.setPublishErr(nil)
	fc.respond("zigbee2mqtt/lamp1/set", "zigbee2mqtt/lamp1", []byte(`{"state":"ON","brightness":254}`))
	done := make(chan error, 1)
	go func() {
		_, err := nc.applyCommand(context.Background(), onCommand("zigbee-bulb"))
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "a fresh command after the storm must go straight through")
	case <-time.After(3 * time.Second):
		t.Fatal("the lamp's command path is wedged after the load")
	}

	nc.mqtt.quiesce() // the health check's own echo is applied asynchronously too
	l := published(t, nc, "zigbee-bulb").GetLight()
	assert.True(t, l.GetOnOff().GetState().GetIsOn())
	assert.GreaterOrEqual(t, l.GetBrightness().GetState().GetLevel(), int32(0))
	assert.LessOrEqual(t, l.GetBrightness().GetState().GetLevel(), int32(100))
}
