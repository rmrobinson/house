-- Built-in system policy "sys.power-restore": when power is restored,
-- re-apply this device's last known light state from the cache.
--
-- HomeAPI has no way to enumerate every device yet (see the "HomeAPI
-- surface" open question in the policy engine plan), so this restores a
-- single well-known entity as an example rather than looping over "all
-- devices". Extend this once that surface exists, or override
-- "sys.power-restore" entirely.
local id = "light.living_room"
local wasOn = home.getLastKnown(id)
if wasOn ~= nil then
    home.setLight(id, wasOn)
end
