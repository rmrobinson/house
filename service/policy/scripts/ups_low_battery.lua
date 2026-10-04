-- User policy "ups-low-battery": emails recipient "r" when a UPS on battery
-- falls to 20% charge or below. One policy covers every UPS. Register
-- against the condition
--   devices.any-match {Kind: "ups", Clauses: [
--     {Key: "battery.state.discharging", Op: "eq", Value: true},
--     {Key: "battery.state.capacity_remaining_pct", Op: "lte", Value: 20}]}
-- with OnConditionFalse = Complete. trigger.device_ids lists the UPS(es)
-- that just crossed the threshold.
for _, id in ipairs(trigger.device_ids) do
    local ok, room = pcall(home.getDeviceRoom, id)
    local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
    local name = home.getDeviceName(id) .. where

    notify.send({
        to = {"r"},
        subject = "[POWER] " .. name .. " battery low",
        body = "<html><body><p>" .. name .. " is on battery and nearly depleted: "
            .. home.getState(id, "battery.state.capacity_remaining_pct") .. "%, about "
            .. home.getState(id, "battery.state.capacity_remaining_mins")
            .. " minutes remaining.</p></body></html>",
        content_type = "text/html",
    })
end
