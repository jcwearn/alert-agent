-- One incident per Alertmanager group while it fires. The alert-agent route
-- groups by alertname and namespace, so the group key identifies "this alert
-- in this namespace". A group that resolves and fires again opens a new
-- incident: the partial unique index only covers firing ones.
CREATE TABLE incidents (
    id           bigserial PRIMARY KEY,
    group_key    text        NOT NULL,
    alertname    text        NOT NULL DEFAULT '',
    namespace    text        NOT NULL DEFAULT '',
    severity     text        NOT NULL DEFAULT '',
    summary      text        NOT NULL DEFAULT '',
    -- Alertmanager's view of the group. The agent's own progress (triaging,
    -- awaiting review, ...) gets a separate column when it exists.
    alert_status text        NOT NULL CHECK (alert_status IN ('firing', 'resolved')),
    opened_at    timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz
);

CREATE UNIQUE INDEX incidents_one_firing_per_group
    ON incidents (group_key) WHERE alert_status = 'firing';
CREATE INDEX incidents_by_opened ON incidents (opened_at DESC);

CREATE TABLE incident_alerts (
    incident_id   bigint      NOT NULL REFERENCES incidents (id),
    fingerprint   text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('firing', 'resolved')),
    labels        jsonb       NOT NULL,
    annotations   jsonb       NOT NULL,
    starts_at     timestamptz NOT NULL,
    ends_at       timestamptz,
    generator_url text        NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (incident_id, fingerprint)
);

-- Every delivery, verbatim, before anything is derived from it. This is the
-- first link in the audit trail: what Alertmanager actually said, and when.
CREATE TABLE webhook_deliveries (
    id          bigserial PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now(),
    group_key   text        NOT NULL,
    status      text        NOT NULL,
    payload     jsonb       NOT NULL,
    -- NULL for a resolve that matches no firing incident, e.g. one that
    -- started firing before this service existed.
    incident_id bigint REFERENCES incidents (id)
);

CREATE INDEX webhook_deliveries_by_incident ON webhook_deliveries (incident_id);
