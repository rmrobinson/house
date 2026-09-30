-- Built-in system policy "sys.occupancy": any motion detected anywhere in
-- the house sets house occupancy to "occupied". Override by re-registering
-- the "sys.occupancy" policy ID with different behaviour.
home.setHouseState("occupancy", "occupied")
