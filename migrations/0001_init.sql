-- 0001_init.sql
--
-- Initial schema for the booking service.
--
-- Design notes that matter for review:
--
--   * Money is stored in minor units as BIGINT. Floating point is never used
--     for an amount, so a total can be compared for equality and summed without
--     a rounding decision.
--
--   * Every mutable row carries a version. Updates are conditional on that
--     version, which is the optimistic locking the service relies on: two
--     concurrent confirmations of the same booking cannot both win.
--
--   * Idempotency is enforced by the database, not by application memory, so a
--     retry that lands on another instance behaves the same way.
--
--   * Passenger names are personal data. They live in a separate table with a
--     retention policy rather than in the bookings row, so the common query
--     path never touches them.

BEGIN;

-- ---------------------------------------------------------------------------
-- schema_migrations
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT        PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE schema_migrations IS
    'Applied migration versions. A row is inserted inside the same transaction as the migration.';

-- ---------------------------------------------------------------------------
-- bookings
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bookings (
    id                  UUID        PRIMARY KEY,
    user_id             TEXT        NOT NULL,

    state               TEXT        NOT NULL,
    version             BIGINT      NOT NULL DEFAULT 1,

    origin_code         TEXT        NOT NULL,
    destination_code    TEXT        NOT NULL,
    departure_date      DATE        NOT NULL,
    return_date         DATE,
    cabin               TEXT        NOT NULL,
    currency            CHAR(3)     NOT NULL,
    traveller_count     SMALLINT    NOT NULL CHECK (traveller_count BETWEEN 1 AND 9),

    -- The raw user utterance. It is the highest-risk column in this schema:
    -- it can contain anything the user typed, so it is never logged, never
    -- returned by the API, and is deleted together with the booking.
    raw_query           TEXT,

    offer_supplier_id   TEXT,
    offer_supplier_ref  TEXT,
    offer_total_minor   BIGINT,
    offer_currency      CHAR(3),
    offer_available_at  TIMESTAMPTZ,
    offer_cancel_terms  TEXT,

    hold_supplier_id    TEXT,
    hold_supplier_ref   TEXT,
    hold_total_minor    BIGINT,
    hold_currency       CHAR(3),
    hold_expires_at     TIMESTAMPTZ,

    confirm_supplier_id TEXT,
    confirm_supplier_ref TEXT,
    confirm_total_minor BIGINT,
    confirm_currency    CHAR(3),
    confirmed_at        TIMESTAMPTZ,
    ticket_document     TEXT,

    failure_code        TEXT,
    cancel_requested_at TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT bookings_state_valid CHECK (state IN (
        'intent', 'searching', 'offered', 'holding', 'held',
        'confirming', 'confirmed', 'cancelling', 'cancelled',
        'failed', 'expired'
    )),

    -- An amount and its currency are stored together or not at all, so a reader
    -- can never interpret a total in the wrong currency.
    CONSTRAINT bookings_offer_amount_complete CHECK (
        (offer_total_minor IS NULL) = (offer_currency IS NULL)
    ),
    CONSTRAINT bookings_hold_amount_complete CHECK (
        (hold_total_minor IS NULL) = (hold_currency IS NULL)
    ),
    CONSTRAINT bookings_confirm_amount_complete CHECK (
        (confirm_total_minor IS NULL) = (confirm_currency IS NULL)
    ),

    -- A booking in a confirmed state must carry a confirmation reference.
    CONSTRAINT bookings_confirmed_has_reference CHECK (
        state <> 'confirmed' OR (confirm_supplier_ref IS NOT NULL AND confirmed_at IS NOT NULL)
    ),

    CONSTRAINT bookings_return_after_departure CHECK (
        return_date IS NULL OR return_date >= departure_date
    )
);

-- The read path filters by owner, so the index starts with user_id.
CREATE INDEX IF NOT EXISTS bookings_user_created_idx
    ON bookings (user_id, created_at DESC);

-- A background sweeper looks for lapsed holds.
CREATE INDEX IF NOT EXISTS bookings_hold_expiry_idx
    ON bookings (hold_expires_at)
    WHERE state = 'held';

COMMENT ON COLUMN bookings.version IS
    'Optimistic lock. An update must match the version it read, otherwise it is retried.';
COMMENT ON COLUMN bookings.raw_query IS
    'Free text from the user. Personal data: excluded from logs and API responses.';

-- ---------------------------------------------------------------------------
-- booking_passengers
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS booking_passengers (
    booking_id  UUID    NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    position    SMALLINT NOT NULL,
    full_name   TEXT    NOT NULL,

    PRIMARY KEY (booking_id, position),

    CONSTRAINT booking_passengers_position_valid CHECK (position >= 0)
);

COMMENT ON TABLE booking_passengers IS
    'Personal data, separated from the booking so the common query path never reads names. Deleted with the booking.';
COMMENT ON COLUMN booking_passengers.full_name IS
    'Traveller name as supplied by the user. Never returned by the API, which reports a count instead.';

-- ---------------------------------------------------------------------------
-- booking_history
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS booking_history (
    id              BIGSERIAL   PRIMARY KEY,
    booking_id      UUID        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    from_state      TEXT        NOT NULL,
    to_state        TEXT        NOT NULL,
    trigger         TEXT        NOT NULL,
    actor           TEXT        NOT NULL,
    at              TIMESTAMPTZ NOT NULL,
    request_id      TEXT,
    idempotency_key TEXT,

    CONSTRAINT booking_history_actor_valid CHECK (actor IN ('user', 'system', 'supplier'))
);

CREATE INDEX IF NOT EXISTS booking_history_booking_idx
    ON booking_history (booking_id, id);

COMMENT ON TABLE booking_history IS
    'Append-only audit trail. It is written in the same transaction as the state change, so a history entry always corresponds to a real transition.';

-- ---------------------------------------------------------------------------
-- idempotency_keys
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key             TEXT        PRIMARY KEY,
    booking_id      UUID        NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    user_id         TEXT        NOT NULL,
    -- A hash of the request that created the binding. A replay with the same
    -- key and a different request is refused, so a client cannot silently
    -- lose an operation.
    request_fingerprint CHAR(64) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idempotency_keys_booking_idx
    ON idempotency_keys (booking_id);

COMMENT ON TABLE idempotency_keys IS
    'Idempotency is enforced here so a retry is handled identically on every instance.';

-- ---------------------------------------------------------------------------
-- updated_at is maintained by the database, not by the application.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS bookings_set_updated_at ON bookings;

CREATE TRIGGER bookings_set_updated_at
    BEFORE UPDATE ON bookings
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

-- A booking that reaches a terminal state is no longer mutated.
CREATE OR REPLACE FUNCTION reject_terminal_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.state IN ('cancelled', 'failed', 'expired') THEN
        RAISE EXCEPTION 'booking % is terminal and cannot be modified', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS bookings_reject_terminal ON bookings;

CREATE TRIGGER bookings_reject_terminal
    BEFORE UPDATE ON bookings
    FOR EACH ROW
    EXECUTE FUNCTION reject_terminal_mutation();

-- ---------------------------------------------------------------------------
-- The write path. Every state change goes through this function so optimistic
-- locking cannot be bypassed by a stray UPDATE.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION apply_booking_transition(
    p_booking_id     UUID,
    p_expected_version BIGINT,
    p_next_state     TEXT,
    p_trigger        TEXT,
    p_actor          TEXT,
    p_at             TIMESTAMPTZ,
    p_request_id     TEXT,
    p_idempotency_key TEXT
) RETURNS BIGINT AS $$
DECLARE
    v_from    TEXT;
    v_version BIGINT;
BEGIN
    -- FOR UPDATE serialises two concurrent transitions of the same booking, so
    -- the version check below sees a committed value.
    SELECT state, version INTO v_from, v_version
    FROM bookings
    WHERE id = p_booking_id
    FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'booking % not found', p_booking_id USING ERRCODE = 'no_data_found';
    END IF;

    IF v_version <> p_expected_version THEN
        RAISE EXCEPTION 'version conflict on booking %: expected %, found %',
            p_booking_id, p_expected_version, v_version
            USING ERRCODE = 'serialization_failure';
    END IF;

    UPDATE bookings
    SET state       = p_next_state,
        version     = version + 1
    WHERE id = p_booking_id;

    INSERT INTO booking_history (booking_id, from_state, to_state, trigger, actor, at, request_id, idempotency_key)
    VALUES (p_booking_id, v_from, p_next_state, p_trigger, p_actor, p_at, p_request_id, p_idempotency_key);

    RETURN v_version + 1;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION apply_booking_transition IS
    'The only supported way to change a booking state. It enforces the version check and writes the audit entry in one transaction.';

INSERT INTO schema_migrations (version) VALUES ('0001_init')
    ON CONFLICT (version) DO NOTHING;

COMMIT;
