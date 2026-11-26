-- Phase 5: the agent's actions on the cluster (delete_pod, rollout_restart,
-- rerun_cronjob, flux_reconcile), the human approvals that its guardrails
-- defer to, and the switch that pauses autonomous actions.
--
-- A cluster action is an actions row with a mode: 'autonomous' when the
-- agent took it, 'approved' when a human approved it in the UI. Rows without
-- a mode are everything else in the audit trail (PRs, merges, links,
-- request_approval itself). The target is the workload the action is about
-- (a pod's Deployment), which the cooldown keys on; object is what the call
-- touched.
ALTER TABLE actions
    ALTER COLUMN incident_id DROP NOT NULL,
    ADD COLUMN mode             text CHECK (mode IN ('autonomous', 'approved')),
    ADD COLUMN target_kind      text NOT NULL DEFAULT '',
    ADD COLUMN target_namespace text NOT NULL DEFAULT '',
    ADD COLUMN target_name      text NOT NULL DEFAULT '',
    ADD COLUMN object           text NOT NULL DEFAULT '',
    ADD COLUMN approval_id      bigint,
    -- Pausing and resuming are about no incident.
    ADD CONSTRAINT actions_has_incident CHECK (incident_id IS NOT NULL OR tool IN ('pause_actions', 'resume_actions'));

CREATE INDEX actions_cluster_by_time ON actions (created_at) WHERE mode IS NOT NULL;

-- An action the agent asked a human to approve. incident_id is the root
-- incident (a merged incident's parent), which holds at most one pending
-- request: a newer one supersedes it.
CREATE TABLE approvals (
    id               bigserial PRIMARY KEY,
    incident_id      bigint      NOT NULL REFERENCES incidents (id),
    run_id           bigint      NOT NULL REFERENCES runs (id),
    tool             text        NOT NULL,
    input            jsonb       NOT NULL,
    reason           text        NOT NULL,
    target_kind      text        NOT NULL DEFAULT '',
    target_namespace text        NOT NULL DEFAULT '',
    target_name      text        NOT NULL DEFAULT '',
    object           text        NOT NULL DEFAULT '',
    status           text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'superseded')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    decided_by       text        NOT NULL DEFAULT '',
    decided_at       timestamptz,
    action_id        bigint      REFERENCES actions (id)
);

CREATE UNIQUE INDEX approvals_one_pending ON approvals (incident_id) WHERE status = 'pending';
CREATE INDEX approvals_by_incident ON approvals (incident_id);

ALTER TABLE actions ADD CONSTRAINT actions_approval FOREIGN KEY (approval_id) REFERENCES approvals (id);

-- Runtime switches the UI can flip. 'actions_paused' holds {"paused": bool}.
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_by text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
