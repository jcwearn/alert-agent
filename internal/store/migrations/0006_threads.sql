-- Phase 4.5: chat moves from "one conversation per PR" to threads. An
-- incident has at most one thread (it resumes the agent's own session for
-- that incident, and PR comments join it); general threads are about the
-- cluster as a whole. Each thread owns one claude session.
CREATE TABLE threads (
    id          bigserial PRIMARY KEY,
    kind        text        NOT NULL CHECK (kind IN ('incident', 'general')),
    incident_id bigint      UNIQUE REFERENCES incidents (id),
    title       text        NOT NULL DEFAULT '',
    -- The claude session the thread continues: for an incident, the run that
    -- opened its PR or else its latest diagnosis; for a general thread, new.
    session_id  uuid        NOT NULL DEFAULT gen_random_uuid(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,
    CHECK ((kind = 'incident') = (incident_id IS NOT NULL))
);

CREATE INDEX threads_recent ON threads (updated_at DESC) WHERE archived_at IS NULL;

-- Every PR so far had its conversation: it becomes its incident's thread.
INSERT INTO threads (kind, incident_id, session_id, created_at, updated_at)
SELECT 'incident', p.incident_id, r.session_id, p.created_at, p.updated_at
FROM pull_requests p JOIN runs r ON r.id = p.run_id;

-- A chat turn belongs to a thread. A general thread's turns have no
-- incident, so incident_id is required only of alert runs.
ALTER TABLE runs ADD COLUMN thread_id bigint REFERENCES threads (id);

UPDATE runs r SET thread_id = t.id
FROM pull_requests p JOIN threads t ON t.incident_id = p.incident_id
WHERE r.kind = 'chat' AND r.pr_id = p.id;

ALTER TABLE runs
    ALTER COLUMN incident_id DROP NOT NULL,
    DROP CONSTRAINT runs_chat_has_pr,
    ADD CONSTRAINT runs_chat_has_thread CHECK (kind <> 'chat' OR thread_id IS NOT NULL),
    ADD CONSTRAINT runs_diagnose_has_incident CHECK (kind <> 'diagnose' OR incident_id IS NOT NULL);

DROP INDEX runs_one_queued_chat_per_pr;
CREATE UNIQUE INDEX runs_one_queued_chat_per_thread ON runs (thread_id) WHERE kind = 'chat' AND status = 'queued';
CREATE INDEX runs_by_thread ON runs (thread_id) WHERE thread_id IS NOT NULL;

-- Messages belong to a thread; pr_id stays for GitHub comments (and is set
-- on the backfilled ones). page is where in the UI a message was sent from.
ALTER TABLE chat_messages
    ADD COLUMN thread_id bigint REFERENCES threads (id),
    ADD COLUMN page      text NOT NULL DEFAULT '';

UPDATE chat_messages m SET thread_id = t.id
FROM pull_requests p JOIN threads t ON t.incident_id = p.incident_id
WHERE m.pr_id = p.id;

ALTER TABLE chat_messages
    ALTER COLUMN thread_id SET NOT NULL,
    ALTER COLUMN pr_id DROP NOT NULL;

CREATE INDEX chat_messages_by_thread ON chat_messages (thread_id, id);
