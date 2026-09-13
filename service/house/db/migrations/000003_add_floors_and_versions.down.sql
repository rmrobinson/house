ALTER TABLE building DROP COLUMN version;

ALTER TABLE room DROP COLUMN version;
ALTER TABLE room DROP COLUMN floor_id;

DROP TABLE floor;
