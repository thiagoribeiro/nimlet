-- +goose Up
-- stream_inflight records stream positions that have been allocated to a send whose
-- transaction has not committed yet. /sync never returns (or advances a cursor past) a
-- position at or above the lowest in-flight one, so events that commit out of stream_pos
-- order can no longer be skipped by a client cursor.
--
-- Allocation happens in its own tiny transaction under an advisory lock, so allocations
-- become visible strictly in stream_pos order; the send's write statement deletes its row
-- atomically with the event insert. UNLOGGED: after a crash every in-flight send is gone
-- anyway, so an empty table is the correct recovered state.
CREATE UNLOGGED TABLE stream_inflight (
    pos          BIGINT      PRIMARY KEY,
    allocated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- +goose StatementBegin
CREATE FUNCTION nimlet_assert(ok BOOLEAN, msg TEXT) RETURNS BOOLEAN
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT ok THEN
        RAISE EXCEPTION '%', msg;
    END IF;
    RETURN TRUE;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION nimlet_assert(BOOLEAN, TEXT);
DROP TABLE stream_inflight;
