-- Built-in system policy "sys.power-restore": when power is restored,
-- re-apply every known light's last known on/off state from the cache -
-- e.g. undoing a light that powered back on by itself (a common firmware
-- default) even though it was off before the outage.
for _, id in ipairs(home.findDevices("light")) do
    local wasOn = home.getLastKnown(id)
    if wasOn ~= nil then
        home.setLight(id, wasOn)
    end
end
