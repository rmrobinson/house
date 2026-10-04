-- User policy "battery-report": a daily summary of every sensor/generic
-- device with a populated Battery trait (UPS excluded - mains-backup
-- battery is a different concern from a battery-powered device dying),
-- partitioned into action items (below 10%) and informational (10% and
-- up), emailed to recipient "r". Not a sys.* default - this is specific to
-- this house's actual device inventory, registered through adminui's
-- policy editor against a "schedule.daily" condition (07:00, house
-- timezone).
--
-- Each entry is suffixed with its linked room, when it has one - several
-- battery device names are otherwise ambiguous (e.g. more than one generic
-- "Motion Sensor") with no way to tell them apart in the email alone.
local function section(title, items)
    if #items == 0 then
        return ""
    end
    return "<h3>" .. title .. "</h3><ul>" .. table.concat(items) .. "</ul>"
end

local action, info = {}, {}

for _, kind in ipairs({"sensor", "generic"}) do
    for _, id in ipairs(home.findDevices(kind)) do
        if home.hasState(id, "battery") then
            local pct = home.getState(id, "battery.state.capacity_remaining_pct")
            local label = home.getDeviceName(id)
            local room = home.getDeviceRoom(id)
            if room ~= "" then
                label = label .. " (" .. room .. ")"
            end
            local entry = "<li>" .. label .. ": " .. pct .. "%</li>"
            if pct < 10 then
                table.insert(action, entry)
            else
                table.insert(info, entry)
            end
        end
    end
end

local html = "<html><body>"
    .. section("Action needed (below 10%)", action)
    .. section("Informational", info)
    .. "</body></html>"

notify.send({ to = {"r"}, subject = "Battery report", body = html, content_type = "text/html" })
