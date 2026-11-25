-- Phase 4: the reviewer talks to the agent about its PR, from the UI or (from
-- 0.10.0) in PR comments. Each turn is a run of kind 'chat' that resumes the
-- PR's claude session (runs.session_id is the PR run's) on the PR branch.
ALTER TABLE runs
    ADD COLUMN kind  text   NOT NULL DEFAULT 'diagnose' CHECK (kind IN ('diagnose', 'chat')),
    ADD COLUMN pr_id bigint REFERENCES pull_requests (id),
    ADD CONSTRAINT runs_chat_has_pr CHECK (kind <> 'chat' OR pr_id IS NOT NULL);

-- Messages that arrive while a turn waits join it; one waiting turn per PR.
CREATE UNIQUE INDEX runs_one_queued_chat_per_pr ON runs (pr_id) WHERE kind = 'chat' AND status = 'queued';
CREATE INDEX runs_by_pr ON runs (pr_id) WHERE pr_id IS NOT NULL;

-- The conversation on a PR: the reviewer's messages (UI or GitHub) and the
-- agent's replies, in order. run_id is the turn that took a message, or that
-- wrote a reply.
CREATE TABLE chat_messages (
    id           bigserial PRIMARY KEY,
    pr_id        bigint      NOT NULL REFERENCES pull_requests (id),
    run_id       bigint      REFERENCES runs (id),
    source       text        NOT NULL CHECK (source IN ('ui', 'github', 'agent')),
    author       text        NOT NULL DEFAULT '',
    body         text        NOT NULL,
    -- queued until its turn ends (answered or failed); capped if a limit
    -- refused it; reply for the agent's own messages.
    status       text        NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'answered', 'failed', 'capped', 'reply')),
    -- GitHub comments: which kind and id (the dedupe key), the thread a line
    -- comment belongs to, and where it points.
    gh_kind      text        CHECK (gh_kind IN ('issue_comment', 'review_comment', 'review')),
    gh_id        bigint,
    gh_thread_id bigint,
    path         text        NOT NULL DEFAULT '',
    line         integer,
    diff_hunk    text        NOT NULL DEFAULT '',
    html_url     text        NOT NULL DEFAULT '',
    reacted_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (gh_kind, gh_id),
    CHECK ((gh_kind IS NULL) = (gh_id IS NULL))
);

CREATE INDEX chat_messages_by_pr ON chat_messages (pr_id, id);
CREATE INDEX chat_messages_by_run ON chat_messages (run_id) WHERE run_id IS NOT NULL;

-- Where the comment poller (0.10.0) reads from. Starting at migration time
-- means comments made before the poller existed are never answered.
ALTER TABLE pull_requests ADD COLUMN comments_since timestamptz NOT NULL DEFAULT now();
