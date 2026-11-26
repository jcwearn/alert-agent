-- The home page pages through incident families: a root incident (not merged
-- into another) and its children. Keyset pagination on the root's
-- (opened_at, id) walks this index; incidents_by_parent finds the children.
CREATE INDEX incidents_roots_by_opened ON incidents (opened_at DESC, id DESC) WHERE parent_id IS NULL;
