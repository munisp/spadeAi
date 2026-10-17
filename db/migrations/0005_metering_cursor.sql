-- High-water mark for spade-metering: every hour <= last_hour has been metered.
CREATE TABLE metering_cursor (
  id         boolean PRIMARY KEY DEFAULT true CHECK (id),
  last_hour  timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
