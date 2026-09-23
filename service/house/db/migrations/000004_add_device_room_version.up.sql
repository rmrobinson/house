-- Backfilled to '1' for rows that predate this column, the same convention
-- migration 000003 used for building/room.version - LinkDevice always mints
-- a fresh UUID version, so '1' only ever appears on pre-existing rows.
ALTER TABLE device_room ADD COLUMN version TEXT NOT NULL DEFAULT '1';
