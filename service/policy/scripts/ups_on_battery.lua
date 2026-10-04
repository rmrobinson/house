-- User policy "ups-on-battery": emails recipient "r" when a UPS loses grid
-- power. One policy covers every UPS. Register against the condition
--   devices.any-match {Kind: "ups",
--     Clauses: [{Key: "battery.state.discharging", Op: "eq", Value: true}]}
-- with OnConditionFalse = Complete. trigger.device_ids lists the UPS(es)
-- that just went on battery (a UPS already on battery isn't repeated).
for _, id in ipairs(trigger.device_ids) do
    -- pcall-guarded: a HomeAPI with no HouseService connection raises from
    -- getDeviceRoom, and a cosmetic room must never abort the alert.
    local ok, room = pcall(home.getDeviceRoom, id)
    local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
    local name = home.getDeviceName(id) .. where

    notify.send({
        to = {"r"},
        subject = "[POWER] " .. name .. " lost grid power",
        body = "<html><body><p>" .. name .. " is running on battery: "
            .. home.getState(id, "battery.state.capacity_remaining_pct") .. "%, about "
            .. home.getState(id, "battery.state.capacity_remaining_mins")
            .. " minutes remaining.</p></body></html>",
        content_type = "text/html",
    })
end
