-- User policy "water-detected": emails recipient "r" when any sensor
-- reports water. One policy covers every sensor. Register against
--   devices.any-match {Kind: "sensor",
--     Clauses: [{Key: "water.is_active", Op: "eq", Value: true}]}
-- with OnConditionFalse = Complete. trigger.device_ids lists the sensor(s)
-- that just detected water. Sensors with no water trait fail the clause
-- (GetState errors) and never match.
for _, id in ipairs(trigger.device_ids) do
    local ok, room = pcall(home.getDeviceRoom, id)
    local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
    local name = home.getDeviceName(id) .. where

    notify.send({
        to = {"r"},
        subject = "[WATER] Water detected: " .. name,
        body = "<html><body><p>" .. name .. " has detected water.</p></body></html>",
        content_type = "text/html",
    })
end
