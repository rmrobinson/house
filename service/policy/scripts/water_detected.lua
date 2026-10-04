-- User policy template "water-detected-<device>": emails recipient "r" when
-- one water sensor trips. One policy per sensor, with DEVICE_ID replaced,
-- against the condition
--   attribute.equals {DeviceID, Key: "water.is_active", Value: true}
local id = "DEVICE_ID"

local ok, room = pcall(home.getDeviceRoom, id)
local where = (ok and room ~= "") and (" (" .. room .. ")") or ""

local name = home.getDeviceName(id) .. where

notify.send({
    to = {"r"},
    subject = "[WATER] Water detected: " .. name,
    body = "<html><body><p>" .. name .. " has detected water.</p></body></html>",
    content_type = "text/html",
})
