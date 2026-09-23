package facade

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/backoffutil"
	"github.com/rmrobinson/house/grpcutil"
)

const (
	minReconnectBackoff = time.Second
	maxReconnectBackoff = 30 * time.Second
)

// upstreamConn manages one upstream BridgeService connection: dialing,
// streaming updates into the Facade's cache, and reconnecting (with jittered
// backoff) on drop. bridgeID is unknown until the first InitialUpdate
// arrives, since addresses - not bridge IDs - are what's configured.
type upstreamConn struct {
	addr string
	f    *Facade

	// backoff is nanoseconds, reset to minReconnectBackoff once the
	// connection has actually delivered a message (see connectOnce) so a
	// brief blip after a long stable connection doesn't pay for backoff
	// accumulated by earlier, unrelated failures - mirrors bridges/lib/
	// webosctrl/conn.go's Conn.backoff. Deliberately not reset on a bare
	// successful Dial - grpcutil.DialInsecure dials lazily and essentially
	// never fails synchronously, so that would prove nothing about whether
	// the upstream is actually reachable.
	backoff atomic.Int64

	mu       sync.Mutex
	bridgeID string
	client   api2.BridgeServiceClient
	live     bool
}

// snapshot returns the current client and whether the connection is live,
// safe to call concurrently with the connection's own goroutine.
func (u *upstreamConn) snapshot() (api2.BridgeServiceClient, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.client, u.live
}

func (u *upstreamConn) connected() bool {
	_, live := u.snapshot()
	return live
}

// run connects to addr and streams updates until ctx is cancelled,
// reconnecting with jittered exponential backoff whenever the connection
// drops.
func (u *upstreamConn) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := u.connectOnce(ctx); err != nil && ctx.Err() == nil {
			u.f.logger.Warn("upstream bridge connection ended, will retry",
				zap.String("addr", u.addr), zap.Error(err))
		}

		backoff := time.Duration(u.backoff.Load())

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffutil.Jitter(backoff)):
		}

		u.backoff.Store(int64(min(backoff*2, maxReconnectBackoff)))
	}
}

func (u *upstreamConn) connectOnce(ctx context.Context) error {
	conn, err := grpcutil.DialInsecure(u.addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := api2.NewBridgeServiceClient(conn)

	u.mu.Lock()
	u.client = client
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		bridgeID := u.bridgeID
		u.live = false
		u.mu.Unlock()
		u.f.markUnreachable(bridgeID)
	}()

	stream, err := client.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
	if err != nil {
		return fmt.Errorf("stream updates: %w", err)
	}

	first := true
	for {
		update, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}

		if first {
			// The connection is only confirmed live once a message has
			// actually been received from it - neither a successful Dial
			// (grpcutil.DialInsecure dials lazily and essentially never
			// fails synchronously) nor a successful StreamUpdates call
			// (which can still return a stream that errors on the first
			// Recv) proves the upstream is reachable. Resetting backoff
			// here, rather than right after Dial, means a
			// persistently-failing upstream actually backs off toward
			// maxReconnectBackoff instead of retrying at the minimum
			// forever.
			u.backoff.Store(int64(minReconnectBackoff))
			first = false
		}

		if iu := update.GetInitialUpdate(); iu != nil {
			u.f.ingestInitial(u, iu)
			continue
		}
		u.f.ingestPassthrough(update)
	}
}

// ingestInitial applies a full-snapshot InitialUpdate from uc's upstream
// bridge, seeding (on first connect) or re-syncing (on reconnect) that
// bridge's device set in the cache, and emits the ADDED/CHANGED/REMOVED
// updates the change implies to already-subscribed downstream clients. The
// raw InitialUpdate itself is never forwarded - see Facade.StreamUpdates.
func (f *Facade) ingestInitial(uc *upstreamConn, iu *api2.InitialUpdate) {
	b := iu.GetBridge()
	if b == nil {
		f.logger.Warn("initial update missing bridge", zap.String("addr", uc.addr))
		return
	}
	bridgeID := b.GetId()
	bClone := proto.Clone(b).(*api2.Bridge)

	f.mu.Lock()

	uc.mu.Lock()
	uc.bridgeID = bridgeID
	uc.live = true
	uc.mu.Unlock()
	f.byID[bridgeID] = uc

	prevBridge, hadBridge := f.bridges[bridgeID]
	f.bridges[bridgeID] = bClone

	// Reconcile this bridge's device set against the new full snapshot:
	// anything previously owned by this bridge but absent now was removed
	// upstream.
	seen := make(map[string]bool, len(iu.GetDevices()))
	var added, changed []*device.Device
	for _, d := range iu.GetDevices() {
		dClone := proto.Clone(d).(*device.Device)
		seen[d.GetId()] = true

		prev, existed := f.devices[d.GetId()]
		f.devices[d.GetId()] = dClone
		f.owner[d.GetId()] = bridgeID

		if !existed {
			added = append(added, dClone)
		} else if !proto.Equal(prev, dClone) {
			changed = append(changed, dClone)
		}
	}
	var removedIDs []string
	for id, owner := range f.owner {
		if owner == bridgeID && !seen[id] {
			removedIDs = append(removedIDs, id)
		}
	}
	for _, id := range removedIDs {
		delete(f.devices, id)
		delete(f.owner, id)
	}

	f.mu.Unlock()

	switch {
	case !hadBridge:
		f.publishBridgeUpdate(api2.Update_ADDED, bridgeID, proto.Clone(bClone).(*api2.Bridge))
	case !proto.Equal(prevBridge, bClone):
		f.publishBridgeUpdate(api2.Update_CHANGED, bridgeID, proto.Clone(bClone).(*api2.Bridge))
	}
	for _, d := range added {
		f.publishDeviceUpdate(api2.Update_ADDED, bridgeID, d.GetId(), f.present(d))
	}
	for _, d := range changed {
		f.publishDeviceUpdate(api2.Update_CHANGED, bridgeID, d.GetId(), f.present(d))
	}
	for _, id := range removedIDs {
		f.publishDeviceUpdate(api2.Update_REMOVED, bridgeID, id, nil)
	}
}

// ingestPassthrough applies a non-initial Update (BridgeUpdate, DeviceUpdate
// or CommandUpdate) to the cache and re-emits it to this facade's own
// subscribers. BridgeUpdate and CommandUpdate already self-identify their
// bridge_id/device_id and carry no per-hop address, so they're forwarded
// unchanged. A DeviceUpdate's Device carries Address, which - like every
// other Device this facade hands out - is rewritten via present() before
// publishing; the cache (f.devices) still keeps the untouched original.
func (f *Facade) ingestPassthrough(u *api2.Update) {
	switch upd := u.Update.(type) {
	case *api2.Update_BridgeUpdate:
		f.mu.Lock()
		id := upd.BridgeUpdate.GetBridgeId()
		if u.Action == api2.Update_REMOVED {
			delete(f.bridges, id)
		} else if b := upd.BridgeUpdate.GetBridge(); b != nil {
			f.bridges[id] = proto.Clone(b).(*api2.Bridge)
		}
		f.mu.Unlock()
		f.updates.SendMessage(u)

	case *api2.Update_DeviceUpdate:
		id := upd.DeviceUpdate.GetDeviceId()
		bridgeID := upd.DeviceUpdate.GetBridgeId()

		if u.Action == api2.Update_REMOVED {
			f.mu.Lock()
			delete(f.devices, id)
			delete(f.owner, id)
			f.mu.Unlock()
			f.publishDeviceUpdate(u.Action, bridgeID, id, nil)
			return
		}

		d := upd.DeviceUpdate.GetDevice()
		if d == nil {
			f.updates.SendMessage(u)
			return
		}

		raw := proto.Clone(d).(*device.Device)
		f.mu.Lock()
		f.devices[id] = raw
		f.owner[id] = bridgeID
		f.mu.Unlock()

		f.publishDeviceUpdate(u.Action, bridgeID, id, f.present(raw))

	case *api2.Update_CommandUpdate:
		f.updates.SendMessage(u)
	}
}

// markUnreachable marks bridgeID and every device it owns as unreachable,
// without dropping them from the cache, and emits the corresponding
// synthetic updates. Called when that bridge's upstream connection drops.
func (f *Facade) markUnreachable(bridgeID string) {
	if bridgeID == "" {
		return
	}

	f.mu.Lock()
	var bridgeChanged *api2.Bridge
	if b, ok := f.bridges[bridgeID]; ok && b.GetIsReachable() {
		b = proto.Clone(b).(*api2.Bridge)
		b.IsReachable = false
		f.bridges[bridgeID] = b
		bridgeChanged = proto.Clone(b).(*api2.Bridge)
	}

	var devicesChanged []*device.Device
	for id, owner := range f.owner {
		if owner != bridgeID {
			continue
		}
		d := f.devices[id]
		if !d.GetAddress().GetIsReachable() {
			continue
		}
		d = proto.Clone(d).(*device.Device)
		d.Address.IsReachable = false
		f.devices[id] = d
		devicesChanged = append(devicesChanged, proto.Clone(d).(*device.Device))
	}
	f.mu.Unlock()

	if bridgeChanged != nil {
		f.publishBridgeUpdate(api2.Update_CHANGED, bridgeID, bridgeChanged)
	}
	for _, d := range devicesChanged {
		f.publishDeviceUpdate(api2.Update_CHANGED, bridgeID, d.GetId(), f.present(d))
	}
}

func (f *Facade) publishBridgeUpdate(action api2.Update_Action, bridgeID string, b *api2.Bridge) {
	f.updates.SendMessage(&api2.Update{
		Action: action,
		Update: &api2.Update_BridgeUpdate{
			BridgeUpdate: &api2.BridgeUpdate{
				BridgeId: bridgeID,
				Bridge:   b,
			},
		},
	})
}

func (f *Facade) publishDeviceUpdate(action api2.Update_Action, bridgeID, deviceID string, d *device.Device) {
	f.updates.SendMessage(&api2.Update{
		Action: action,
		Update: &api2.Update_DeviceUpdate{
			DeviceUpdate: &api2.DeviceUpdate{
				BridgeId: bridgeID,
				DeviceId: deviceID,
				Device:   d,
			},
		},
	})
}
