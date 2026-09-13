CREATE TABLE IF NOT EXISTS floor(
    id TEXT PRIMARY KEY,
    name TEXT,
    sort_order INT,
    building_id TEXT NOT NULL,
    version TEXT NOT NULL,
    FOREIGN KEY(building_id) REFERENCES building(id)
);

ALTER TABLE room ADD COLUMN floor_id TEXT REFERENCES floor(id);
-- Backfilled to '1' for rows that predate this column, and required (NOT
-- NULL) from here on - CreateRoom always mints a fresh UUID version, so '1'
-- only ever appears on pre-existing legacy rows.
ALTER TABLE room ADD COLUMN version TEXT NOT NULL DEFAULT '1';

ALTER TABLE building ADD COLUMN version TEXT NOT NULL DEFAULT '1';
