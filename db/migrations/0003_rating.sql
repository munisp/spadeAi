ALTER TABLE meter_reading ADD COLUMN rated_at timestamptz;
CREATE INDEX meter_reading_unrated_idx ON meter_reading(created_at) WHERE rated_at IS NULL;
ALTER TABLE transfer_intent ADD COLUMN settles numeric(39,0);
ALTER TABLE organization ADD COLUMN suspended_at timestamptz;
