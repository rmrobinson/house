-- User policy "battery-report": a daily summary of every sensor/generic
-- device with a populated Battery trait (UPS excluded - mains-backup
-- battery is a different concern from a battery-powered device dying),
-- partitioned into action items (below 10%) and informational (10% and
-- up), emailed to recipient "r". Not a sys.* default - this is specific to
-- this house's actual device inventory, registered through adminui's
-- policy editor against a "schedule.daily" condition (07:00, house
-- timezone).
--
-- It also lists connectivity problems across every light/sensor/generic/fan
-- device: ones whose bridge reports them unreachable, and ones last heard
-- from over 24h ago. A device whose bridge reports no last_seen at all is
-- not listed as stale - "unknown" isn't evidence of silence, and bridges
-- that never set it would otherwise flood the report.
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

-- roomSuffix is pcall-guarded: home.getDeviceRoom raises a Lua error (not
-- just a "" result) on a HomeAPI with no HouseService connection
-- (bridgehome.Adapter's unconditional ErrNotImplemented) - letting that
-- propagate would abort this whole report over a purely cosmetic
-- disambiguation feature, so any error here is treated the same as "no
-- room", not a reason to stop.
local function roomSuffix(id)
    local ok, room = pcall(home.getDeviceRoom, id)
    if ok and room ~= "" then
        return " (" .. room .. ")"
    end
    return ""
end

local action, info = {}, {}

for _, kind in ipairs({"sensor", "generic"}) do
    for _, id in ipairs(home.findDevices(kind)) do
        if home.hasState(id, "battery") then
            local pct = home.getState(id, "battery.state.capacity_remaining_pct")
            local label = home.getDeviceName(id) .. roomSuffix(id)
            local entry = "<li>" .. label .. ": " .. pct .. "%</li>"
            if pct < 10 then
                table.insert(action, entry)
            else
                table.insert(info, entry)
            end
        end
    end
end

-- A device can be listed under only one connectivity section: unreachable
-- wins, since "stale" is just a weaker version of the same problem.
local STALE_AFTER_SECONDS = 24 * 60 * 60
local unreachable, stale = {}, {}

local function ago(seconds)
    if seconds == nil then
        return "never reported"
    end
    local hours = math.floor(seconds / 3600)
    if hours < 48 then
        return hours .. "h ago"
    end
    return math.floor(hours / 24) .. "d ago"
end

for _, kind in ipairs({"light", "sensor", "generic", "fan"}) do
    for _, id in ipairs(home.findDevices(kind)) do
        -- pcall-guarded per device: one that vanished from the cache between
        -- findDevices and these reads must not abort the whole report.
        pcall(function()
            local seconds = home.secondsSinceSeen(id)
            local label = home.getDeviceName(id) .. roomSuffix(id)
            if not home.isReachable(id) then
                table.insert(unreachable, "<li>" .. label .. " (last seen " .. ago(seconds) .. ")</li>")
            elseif seconds ~= nil and seconds > STALE_AFTER_SECONDS then
                table.insert(stale, "<li>" .. label .. " (last seen " .. ago(seconds) .. ")</li>")
            end
        end)
    end
end

local html = "<html><body>"
    .. section("Action needed (below 10%)", action)
    .. section("Informational", info)
    .. section("Unreachable", unreachable)
    .. section("No activity in 24h", stale)
    .. "</body></html>"

notify.send({ to = {"r"}, subject = "Battery report", body = html, content_type = "text/html" })
