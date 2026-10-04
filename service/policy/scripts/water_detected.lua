-- User policy "water-detected": emails recipient "r" when any sensor
-- reports water, naming every sensor currently wet. Register against
--   devices.any-match {Kind: "sensor",
--     Clauses: [{Key: "water.is_active", Op: "eq", Value: true}]}
-- with OnConditionFalse = Complete. See ups_on_battery.lua for why.
-- Sensors with no water trait fail the clause (GetState errors) and are
-- simply not in the matching set; hasState guards the same thing here.
local items = {}

for _, id in ipairs(home.findDevices("sensor")) do
    if home.hasState(id, "water") and home.getState(id, "water.is_active") == true then
        local ok, room = pcall(home.getDeviceRoom, id)
        local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
        table.insert(items, "<li>" .. home.getDeviceName(id) .. where .. "</li>")
    end
end

if #items > 0 then
    notify.send({
        to = {"r"},
        subject = "[WATER] Water detected (" .. #items .. " sensor)",
        body = "<html><body><p>Water detected by:</p><ul>" .. table.concat(items) .. "</ul></body></html>",
        content_type = "text/html",
    })
end
