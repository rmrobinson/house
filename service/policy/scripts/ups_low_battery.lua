-- User policy "ups-low-battery": emails recipient "r" when any UPS on
-- battery is at or below 20% charge, naming every such UPS. Register
-- against the condition
--   devices.any-match {Kind: "ups", Clauses: [
--     {Key: "battery.state.discharging", Op: "eq", Value: true},
--     {Key: "battery.state.capacity_remaining_pct", Op: "lte", Value: 20}]}
-- with OnConditionFalse = Complete. See ups_on_battery.lua for why.
local items = {}

for _, id in ipairs(home.findDevices("ups")) do
    local pct = home.getState(id, "battery.state.capacity_remaining_pct")
    if home.getState(id, "battery.state.discharging") == true and pct <= 20 then
        local ok, room = pcall(home.getDeviceRoom, id)
        local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
        table.insert(items, "<li>" .. home.getDeviceName(id) .. where .. ": " .. pct
            .. "%, about " .. home.getState(id, "battery.state.capacity_remaining_mins")
            .. " minutes remaining</li>")
    end
end

if #items > 0 then
    notify.send({
        to = {"r"},
        subject = "[POWER] UPS battery low (" .. #items .. " UPS)",
        body = "<html><body><p>Nearly out of battery:</p><ul>" .. table.concat(items) .. "</ul></body></html>",
        content_type = "text/html",
    })
end
