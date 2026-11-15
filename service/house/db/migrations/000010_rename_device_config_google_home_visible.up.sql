-- Renames google_home_hidden to google_home_visible and inverts its
-- polarity, not just the name: visibility is opt-in now, so the column's
-- zero value (0/false) must mean hidden, the same direction as "no row at
-- all" already means in code (see service/house/deviceconfig.go). SQLite
-- has no single rename-and-recompute statement, so this adds the new
-- column (every existing row - including ones that only exist because of
-- an unrelated rename, never having had Google Home visibility explicitly
-- set - defaults to 0/hidden via DEFAULT, achieving the same "everything
-- defaults to hidden" sweep the opt-out design needed a separate UPDATE
-- for) and drops the old one.
ALTER TABLE device_config ADD COLUMN google_home_visible INTEGER NOT NULL DEFAULT 0;
ALTER TABLE device_config DROP COLUMN google_home_hidden;
