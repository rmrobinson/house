-- Example user policy: when the house becomes occupied (Building.State.
-- occupied, computed server-side by house service from room motion),
-- switch to "home" mode unless it's already set. Not auto-registered -
-- paste into adminui's policy editor against a condition expression using
-- the "sys.occupied"/"sys.any-motion-detected" condition types (see
-- RegisterSystemConditionTypes) to get this behaviour.
if home.getHouseState("mode") ~= "home" then
    home.setHouseState("mode", "home")
end
