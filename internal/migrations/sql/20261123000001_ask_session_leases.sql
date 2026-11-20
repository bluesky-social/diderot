-- A running Ask turn's hold on its session, so that no two replicas run turns
-- of one session at once. The holder renews expires_at while its turn runs
-- and deletes the row when the turn ends; a row past expires_at belongs to a
-- process that stopped renewing it, and the session's next turn takes it
-- over.
CREATE TABLE ask_session_leases (
    session_id uuid PRIMARY KEY REFERENCES ask_sessions(id) ON DELETE CASCADE,
    holder     uuid NOT NULL,
    expires_at timestamptz NOT NULL
);
