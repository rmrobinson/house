package db

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

// DeviceConfig is an admin-set override for a device's display name and/or
// its Google Home visibility, layered over whatever the owning bridge
// reports - see house.DeviceConfigOverlay. Unlike Device (the room link),
// this carries no Version of its own: a write here is always a
// last-write-wins update, and any optimistic-concurrency check belongs
// against the real Device.version read from the bridge, not against this
// table (see DeviceConfigOverlay.UpdateDeviceConfig).
type DeviceConfig struct {
	ID                string
	Name              string
	GoogleHomeVisible bool
	// PresenceTracked marks the device as counting toward building occupancy.
	PresenceTracked bool
}

// SetDeviceConfig upserts cfg.ID's override - every caller is expected to
// supply every field's current desired value (read fresh via GetDevice
// first), the same whole-object-write convention as the rest of this
// table's callers, so there's no partial-update ambiguity to resolve here.
func (db *Database) SetDeviceConfig(ctx context.Context, cfg DeviceConfig) (*DeviceConfig, error) {
	_, err := db.db.ExecContext(ctx,
		`INSERT INTO device_config (id, name, google_home_visible, presence_tracked) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, google_home_visible=excluded.google_home_visible, presence_tracked=excluded.presence_tracked`,
		cfg.ID, cfg.Name, cfg.GoogleHomeVisible, cfg.PresenceTracked)
	if err != nil {
		db.logger.Error("unable to set device config", zap.String("device_id", cfg.ID), zap.Error(err))
		return nil, err
	}
	return &cfg, nil
}

// GetDeviceConfig retrieves deviceID's stored override, or nil if none has
// ever been set.
func (db *Database) GetDeviceConfig(ctx context.Context, deviceID string) (*DeviceConfig, error) {
	cfg := &DeviceConfig{}
	row := db.db.QueryRowContext(ctx, "SELECT id, name, google_home_visible, presence_tracked FROM device_config WHERE id=?", deviceID)

	if err := row.Scan(&cfg.ID, &cfg.Name, &cfg.GoogleHomeVisible, &cfg.PresenceTracked); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		db.logger.Error("unable to retrieve device config", zap.String("device_id", deviceID), zap.Error(err))
		return nil, err
	}
	return cfg, nil
}

// ListDeviceConfigs retrieves every stored override, keyed by device ID -
// used to overlay a whole ListDevices response in one query rather than one
// GetDeviceConfig per device.
func (db *Database) ListDeviceConfigs(ctx context.Context) (map[string]DeviceConfig, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT id, name, google_home_visible, presence_tracked FROM device_config")
	if err != nil {
		db.logger.Error("unable to list device configs", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	out := map[string]DeviceConfig{}
	for rows.Next() {
		var cfg DeviceConfig
		if err := rows.Scan(&cfg.ID, &cfg.Name, &cfg.GoogleHomeVisible, &cfg.PresenceTracked); err != nil {
			db.logger.Error("unable to scan device config row", zap.Error(err))
			return nil, err
		}
		out[cfg.ID] = cfg
	}
	if err := rows.Err(); err != nil {
		db.logger.Error("error iterating device config rows", zap.Error(err))
		return nil, err
	}
	return out, nil
}
