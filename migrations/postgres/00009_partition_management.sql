-- +goose Up
CREATE TABLE events_p1 PARTITION OF events
    FOR VALUES FROM (1) TO (10000001);

CREATE TABLE event_visibility_p1 PARTITION OF event_visibility
    FOR VALUES FROM (1) TO (10000001);

CREATE TABLE events_p2 PARTITION OF events
    FOR VALUES FROM (10000001) TO (20000001);

CREATE TABLE event_visibility_p2 PARTITION OF event_visibility
    FOR VALUES FROM (10000001) TO (20000001);

CREATE TABLE events_default           PARTITION OF events           DEFAULT;
CREATE TABLE event_visibility_default PARTITION OF event_visibility DEFAULT;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_event_partitions(
    partition_num  BIGINT,
    partition_size BIGINT DEFAULT 10000000
) RETURNS void AS $$
DECLARE
    lower_bound BIGINT := (partition_num - 1) * partition_size + 1;
    upper_bound BIGINT := partition_num       * partition_size + 1;
    ev_name  TEXT := 'events_p'           || partition_num;
    vis_name TEXT := 'event_visibility_p' || partition_num;
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF events FOR VALUES FROM (%L) TO (%L)',
        ev_name, lower_bound, upper_bound
    );
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF event_visibility FOR VALUES FROM (%L) TO (%L)',
        vis_name, lower_bound, upper_bound
    );
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS create_event_partitions(BIGINT, BIGINT);
DROP TABLE IF EXISTS events_default;
DROP TABLE IF EXISTS event_visibility_default;
DROP TABLE IF EXISTS events_p2;
DROP TABLE IF EXISTS event_visibility_p2;
DROP TABLE IF EXISTS events_p1;
DROP TABLE IF EXISTS event_visibility_p1;
