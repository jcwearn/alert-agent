-- Phase 3.5: related alerts that fire together become one run, and the agent
-- (or a human) can merge duplicate incidents or link related ones.

-- A merged incident points at the incident that owns the problem. Merges are
-- one level deep: merging an incident that has children moves them too. The
-- child keeps its own alerts, runs and history.
ALTER TABLE incidents
    ADD COLUMN parent_id    bigint REFERENCES incidents (id),
    ADD COLUMN merged_at    timestamptz,
    -- 'run:<id>' for the agent, 'ui' for a human.
    ADD COLUMN merged_by    text NOT NULL DEFAULT '',
    ADD COLUMN merge_reason text NOT NULL DEFAULT '',
    ADD CONSTRAINT incidents_not_own_parent CHECK (parent_id <> id);

CREATE INDEX incidents_by_parent ON incidents (parent_id) WHERE parent_id IS NOT NULL;

-- Related, not duplicate. Undirected: the lower id is always a.
CREATE TABLE incident_links (
    a          bigint      NOT NULL REFERENCES incidents (id),
    b          bigint      NOT NULL REFERENCES incidents (id),
    reason     text        NOT NULL,
    actor      text        NOT NULL,
    run_id     bigint      REFERENCES runs (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (a, b),
    CHECK (a < b)
);

CREATE INDEX incident_links_by_b ON incident_links (b);

-- The audit trail now has human entries (no run) and entries that touch a
-- second incident (the merge target, the linked incident).
ALTER TABLE actions
    ALTER COLUMN run_id DROP NOT NULL,
    ADD COLUMN actor              text   NOT NULL DEFAULT 'agent',
    ADD COLUMN target_incident_id bigint REFERENCES incidents (id),
    ADD CONSTRAINT actions_actor CHECK (actor IN ('agent', 'ui')),
    ADD CONSTRAINT actions_agent_has_run CHECK (actor <> 'agent' OR run_id IS NOT NULL);

CREATE INDEX actions_by_target ON actions (target_incident_id) WHERE target_incident_id IS NOT NULL;

-- A new run waits until hold_until, and incidents that open in the same
-- namespace meanwhile join it. runs.incident_id stays the primary incident.
ALTER TABLE runs ADD COLUMN hold_until timestamptz;

CREATE TABLE run_incidents (
    run_id      bigint      NOT NULL REFERENCES runs (id),
    incident_id bigint      NOT NULL REFERENCES incidents (id),
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, incident_id)
);

CREATE INDEX run_incidents_by_incident ON run_incidents (incident_id);

INSERT INTO run_incidents (run_id, incident_id, added_at)
SELECT id, incident_id, created_at FROM runs;

-- The files a PR changes, so a second proposal touching them is refused.
ALTER TABLE pull_requests ADD COLUMN files text[] NOT NULL DEFAULT '{}';
