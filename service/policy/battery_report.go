package policy

import _ "embed"

//go:embed scripts/battery_report.lua
var BatteryReportScript string

// BatteryReportPolicyID is the suggested policy ID for registering
// BatteryReportScript - not loaded automatically by LoadDefaultSystemPolicies
// (unlike the sys.* scripts in defaults.go): this is a user policy specific
// to this house's actual device inventory, registered through adminui's
// policy editor against a "schedule.daily" condition (07:00, house
// timezone) once the notification service is deployed and configured.
const BatteryReportPolicyID = "battery-report"
