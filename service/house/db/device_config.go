package db

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

// DeviceConfig is an admin-set override for a device's display name, layered
// over whatever name the owning bridge reports - see
// house.DeviceConfigOverlay. Unlike Device (the room link), this carries no
// Version of its own: a write here is always a last-write-wins rename, and
// any optimistic-concurrency check belongs against the real Device.version
// read from the bridge, not against this table (see DeviceConfigOverlay.
// UpdateDeviceConfig).
type DeviceConfig struct {
	ID   string
	Name string
}

// SetDeviceConfig upserts deviceID's name override.
func (db *Database) SetDeviceConfig(ctx context.Context, deviceID, name string) (*DeviceConfig, error) {
	_, err := db.db.ExecContext(ctx,
		`INSERT INTO device_config (id, name) VALUES (?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		deviceID, name)
	if err != nil {
		db.logger.Error("unable to set device config", zap.String("device_id", deviceID), zap.Error(err))
		return nil, err
	}
	return &DeviceConfig{ID: deviceID, Name: name}, nil
}

// GetDeviceConfig retrieves deviceID's name override, or nil if none has
// ever been set.
func (db *Database) GetDeviceConfig(ctx context.Context, deviceID string) (*DeviceConfig, error) {
	cfg := &DeviceConfig{}
	row := db.db.QueryRowContext(ctx, "SELECT id, name FROM device_config WHERE id=?", deviceID)

	if err := row.Scan(&cfg.ID, &cfg.Name); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		db.logger.Error("unable to retrieve device config", zap.String("device_id", deviceID), zap.Error(err))
		return nil, err
	}
	return cfg, nil
}

// ListDeviceConfigs retrieves every stored name override, keyed by device
// ID - used to overlay a whole ListDevices response in one query rather
// than one GetDeviceConfig per device.
func (db *Database) ListDeviceConfigs(ctx context.Context) (map[string]DeviceConfig, error) {
	rows, err := db.db.QueryContext(ctx, "SELECT id, name FROM device_config")
	if err != nil {
		db.logger.Error("unable to list device configs", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	out := map[string]DeviceConfig{}
	for rows.Next() {
		var cfg DeviceConfig
		if err := rows.Scan(&cfg.ID, &cfg.Name); err != nil {
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
