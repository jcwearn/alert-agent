-- One agent run per incident attempt. In Phase 2 a run is a diagnosis: a Job
-- runs `claude -p` read-only and reports back.
CREATE TABLE runs (
    id          bigserial PRIMARY KEY,
    incident_id bigint      NOT NULL REFERENCES incidents (id),
    status      text        NOT NULL
        CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'skipped')),
    -- Chosen up front and passed to `claude --session-id`, so a later phase can
    -- resume the same conversation.
    session_id  uuid        NOT NULL DEFAULT gen_random_uuid(),
    -- sha256 of the run's bearer token for the runner API. Cleared when the run
    -- finishes, so a leaked token stops working the moment its run is over.
    token_hash  bytea,
    job_name    text        NOT NULL DEFAULT '',
    model       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    report      text        NOT NULL DEFAULT '',
    error       text        NOT NULL DEFAULT '',
    num_turns   integer,
    duration_ms bigint,
    usage       jsonb
);

CREATE INDEX runs_by_incident ON runs (incident_id);
CREATE UNIQUE INDEX runs_token_hash ON runs (token_hash) WHERE token_hash IS NOT NULL;
-- At most one run at a time, enforced by the database rather than by the
-- dispatcher's good behaviour: runs share one subscription's usage limits.
CREATE UNIQUE INDEX runs_one_running ON runs ((true)) WHERE status = 'running';

-- The run's transcript: every line `claude -p --output-format stream-json`
-- printed, in order. seq comes from the runner, so a retried batch is
-- idempotent.
CREATE TABLE run_events (
    run_id      bigint      NOT NULL REFERENCES runs (id),
    seq         integer     NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    type        text        NOT NULL,
    payload     jsonb       NOT NULL,
    PRIMARY KEY (run_id, seq)
);
