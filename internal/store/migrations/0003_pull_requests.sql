-- Every call the agent makes to a controller tool, recorded before the tool
-- does anything and completed with its result. Phase 3 has one such tool
-- (propose_pr); Phase 5's autonomous actions land in the same table.
CREATE TABLE actions (
    id          bigserial PRIMARY KEY,
    run_id      bigint      NOT NULL REFERENCES runs (id),
    incident_id bigint      NOT NULL REFERENCES incidents (id),
    tool        text        NOT NULL,
    input       jsonb       NOT NULL,
    status      text        NOT NULL DEFAULT 'started'
        CHECK (status IN ('started', 'succeeded', 'rejected', 'failed')),
    result      text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE INDEX actions_by_run ON actions (run_id);
CREATE INDEX actions_by_incident ON actions (incident_id);

-- A pull request the controller opened for an incident. One per incident: a
-- later revision is a new commit on the same branch (Phase 4's update_pr).
CREATE TABLE pull_requests (
    id           bigserial PRIMARY KEY,
    incident_id  bigint      NOT NULL UNIQUE REFERENCES incidents (id),
    run_id       bigint      NOT NULL REFERENCES runs (id),
    number       integer     NOT NULL,
    url          text        NOT NULL,
    title        text        NOT NULL,
    branch       text        NOT NULL,
    base_sha     text        NOT NULL,
    head_sha     text        NOT NULL,
    state        text        NOT NULL DEFAULT 'open'
        CHECK (state IN ('open', 'merged', 'closed')),
    -- Rollup of the head commit's check runs: pending, success or failure.
    checks       text        NOT NULL DEFAULT 'pending',
    -- When the "needs your review" push went out; NULL until it has.
    notified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    polled_at    timestamptz,
    closed_at    timestamptz
);

CREATE INDEX pull_requests_open ON pull_requests (created_at) WHERE state = 'open';

-- The claude session file of a finished run, gzipped, so a later phase can
-- restore it and resume the same conversation.
CREATE TABLE run_sessions (
    run_id      bigint      PRIMARY KEY REFERENCES runs (id),
    jsonl_gz    bytea       NOT NULL,
    size        integer     NOT NULL,
    uploaded_at timestamptz NOT NULL DEFAULT now()
);
