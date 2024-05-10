CREATE TABLE IF NOT EXISTS device_room(
    id TEXT PRIMARY KEY,
    room_id TEXT,
    FOREIGN KEY(room_id) REFERENCES room(id)
);
