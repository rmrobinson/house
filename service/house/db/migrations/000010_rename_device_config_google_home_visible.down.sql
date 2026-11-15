-- Not a true inverse - whatever each row's actual google_home_visible value
-- was is discarded along with the column; every row comes back with the old
-- column's default (0 = not hidden = visible), the closest meaningful undo
-- available, same accepted tradeoff as migration 000009's own down.sql.
ALTER TABLE device_config ADD COLUMN google_home_hidden INTEGER NOT NULL DEFAULT 0;
ALTER TABLE device_config DROP COLUMN google_home_visible;
