-- Opt-in flag marking a device (e.g. a person's phone) whose network
-- reachability counts toward building occupancy. Defaults to 0/untracked,
-- including for every existing row.
ALTER TABLE device_config ADD COLUMN presence_tracked INTEGER NOT NULL DEFAULT 0;
