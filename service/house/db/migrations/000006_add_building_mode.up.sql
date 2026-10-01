-- mode and available_modes are NULL on any building created before this
-- migration - scanBuilding treats NULL mode as "" (never set) and NULL
-- available_modes as an empty list (no mode currently settable), the same
-- tolerant-of-pre-migration-NULL stance 000005 took for room aggregation.
ALTER TABLE building ADD COLUMN mode TEXT;
-- JSON-encoded []string, matching the encoding/json-in-a-TEXT-column
-- convention service/policy already uses for structured values (see
-- policy.go's MarshalConditionExpr) - comma-joining would break on a mode
-- name containing a comma, however unlikely.
ALTER TABLE building ADD COLUMN available_modes TEXT;
