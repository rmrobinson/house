-- All five nullable together: a room with no configured override has every
-- column NULL; one with an override has every column set (defaulting an
-- individual unconfigured metric to 0/AggregationUnspecified, not NULL) -
-- see db.Room.Aggregation.
ALTER TABLE room ADD COLUMN agg_occupancy_strategy INTEGER;
ALTER TABLE room ADD COLUMN agg_temperature_strategy INTEGER;
ALTER TABLE room ADD COLUMN agg_light_strategy INTEGER;
ALTER TABLE room ADD COLUMN agg_air_quality_strategy INTEGER;
ALTER TABLE room ADD COLUMN agg_power_strategy INTEGER;
