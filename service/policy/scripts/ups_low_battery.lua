-- User policy template "ups-low-battery-<device>": emails recipient "r" when
-- one UPS, while on battery, falls to 20% charge. One policy per UPS, with
-- DEVICE_ID replaced, against the condition
--   and(
--     attribute.equals    {DeviceID, Key: "battery.state.discharging", Value: true},
--     attribute.threshold {DeviceID, Key: "battery.state.capacity_remaining_pct",
--                          High: 25, Low: 20, Falling: true})
-- The 25/20 hysteresis keeps a reading hovering around 20% from re-alerting.
local id = "DEVICE_ID"

local ok, room = pcall(home.getDeviceRoom, id)
local where = (ok and room ~= "") and (" (" .. room .. ")") or ""

local name = home.getDeviceName(id) .. where
local pct = home.getState(id, "battery.state.capacity_remaining_pct")
local mins = home.getState(id, "battery.state.capacity_remaining_mins")

notify.send({
    to = {"r"},
    subject = "[POWER] " .. name .. " battery low",
    body = "<html><body><p>" .. name .. " is on battery and nearly depleted: "
        .. pct .. "%, about " .. mins .. " minutes remaining.</p></body></html>",
    content_type = "text/html",
})
