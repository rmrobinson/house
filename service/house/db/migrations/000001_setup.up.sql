CREATE TABLE IF NOT EXISTS building(
    id TEXT PRIMARY KEY,
    name TEXT,
    tz TEXT,
    lat REAL,
    lon REAL
);

CREATE TABLE IF NOT EXISTS room(
    id TEXT PRIMARY KEY,
    building_id TEXT,
    name TEXT,
    type INT,
    FOREIGN KEY(building_id) REFERENCES building(id)
);
