-- Built-in system policy "sys.occupancy": when the house becomes occupied
-- (Building.State.occupied, computed server-side by house service from room
-- motion), switch to "home" mode unless it's already set. Override by
-- re-registering the "sys.occupancy" policy ID with different behaviour.
if home.getHouseState("mode") ~= "home" then
    home.setHouseState("mode", "home")
end
