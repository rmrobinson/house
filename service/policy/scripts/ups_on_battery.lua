-- User policy template "ups-on-battery-<device>": emails recipient "r" when
-- one UPS loses grid power. One policy is registered per UPS through
-- adminui/SavePolicy, each with DEVICE_ID below replaced by that UPS's
-- device id and a condition of
--   attribute.equals {DeviceID, Key: "battery.state.discharging", Value: true}
-- Not a sys.* default - specific to this house's device inventory.
local id = "DEVICE_ID"

-- pcall-guarded for the same reason as battery_report.lua: a HomeAPI with
-- no HouseService connection raises from getDeviceRoom, and a cosmetic room
-- suffix must never abort the alert.
local ok, room = pcall(home.getDeviceRoom, id)
local where = (ok and room ~= "") and (" (" .. room .. ")") or ""

local name = home.getDeviceName(id) .. where
local pct = home.getState(id, "battery.state.capacity_remaining_pct")
local mins = home.getState(id, "battery.state.capacity_remaining_mins")

notify.send({
    to = {"r"},
    subject = "[POWER] " .. name .. " lost grid power",
    body = "<html><body><p>" .. name .. " is running on battery.</p><p>Battery: "
        .. pct .. "%, about " .. mins .. " minutes remaining.</p></body></html>",
    content_type = "text/html",
})
