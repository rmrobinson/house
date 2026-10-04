-- User policy "ups-on-battery": emails recipient "r" when any UPS loses
-- grid power, naming every UPS currently on battery. One policy covers all
-- UPSes. Register against the condition
--   devices.any-match {Kind: "ups",
--     Clauses: [{Key: "battery.state.discharging", Op: "eq", Value: true}]}
-- with OnConditionFalse = Complete (the condition pulses false/true when a
-- second UPS newly joins the set, which must not interrupt a running alert).
-- The script gets no trigger context, so it re-queries which UPSes match.
local items = {}

for _, id in ipairs(home.findDevices("ups")) do
    if home.getState(id, "battery.state.discharging") == true then
        -- pcall-guarded: a HomeAPI with no HouseService connection raises
        -- from getDeviceRoom, and a cosmetic room must never abort the alert.
        local ok, room = pcall(home.getDeviceRoom, id)
        local where = (ok and room ~= "") and (" (" .. room .. ")") or ""
        table.insert(items, "<li>" .. home.getDeviceName(id) .. where .. ": "
            .. home.getState(id, "battery.state.capacity_remaining_pct") .. "%, about "
            .. home.getState(id, "battery.state.capacity_remaining_mins") .. " minutes remaining</li>")
    end
end

if #items > 0 then
    notify.send({
        to = {"r"},
        subject = "[POWER] Lost grid power (" .. #items .. " UPS)",
        body = "<html><body><p>Running on battery:</p><ul>" .. table.concat(items) .. "</ul></body></html>",
        content_type = "text/html",
    })
end
