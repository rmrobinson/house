package facade

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridgeconn"
)

// upstreamConn manages one upstream BridgeService connection: dialing and
// streaming (delegated to a bridgeconn.Conn) and the extra bookkeeping the
// Facade needs on top - bridgeID (unknown until the first InitialUpdate
// arrives, since addresses, not bridge IDs, are what's configured) and live,
// which additionally requires that InitialUpdate to have actually arrived on
// the current connection, not just a dial having succeeded.
type upstreamConn struct {
	addr string
	f    *Facade
	conn *bridgeconn.Conn

	mu       sync.Mutex
	bridgeID string
	live     bool
}

func newUpstreamConn(f *Facade, addr string) *upstreamConn {
	return &upstreamConn{
		addr: addr,
		f:    f,
		conn: bridgeconn.New(f.logger, addr),
	}
}

// snapshot returns the current client and whether the connection is live,
// safe to call concurrently with the connection's own goroutine.
func (u *upstreamConn) snapshot() (api2.BridgeServiceClient, bool) {
	u.mu.Lock()
	live := u.live
	u.mu.Unlock()
	return u.conn.Client(), live
}

func (u *upstreamConn) connected() bool {
	_, live := u.snapshot()
	return live
}

// run connects to addr and streams updates until ctx is cancelled,
// reconnecting with jittered exponential backoff whenever the connection
// drops.
func (u *upstreamConn) run(ctx context.Context) {
	u.conn.Run(ctx, u.handleUpdate, u.onDrop)
}

func (u *upstreamConn) handleUpdate(update *api2.Update) {
	if iu := update.GetInitialUpdate(); iu != nil {
		u.f.ingestInitial(u, iu)
		return
	}
	u.f.ingestPassthrough(update)
}

// onDrop marks this connection no longer live and flags its bridge (and
// devices) unreachable. Called once each time the underlying connection is
// lost, including on shutdown while connected.
func (u *upstreamConn) onDrop() {
	u.mu.Lock()
	bridgeID := u.bridgeID
	u.live = false
	u.mu.Unlock()
	u.f.markUnreachable(bridgeID)
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
