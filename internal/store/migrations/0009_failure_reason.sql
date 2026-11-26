-- Why a failed run failed, when the reason will fail every run the same way
-- until a person or the clock fixes it: 'auth' (the Claude token) or
-- 'usage_limit' (the subscription's limit). Such a run is deferred: a new run
-- is queued, held until a retry time, and the dispatcher starts nothing while
-- the block (settings key 'claude_blocked') stands.
ALTER TABLE runs ADD COLUMN failure_reason text NOT NULL DEFAULT ''
    CHECK (failure_reason IN ('', 'auth', 'usage_limit'));
