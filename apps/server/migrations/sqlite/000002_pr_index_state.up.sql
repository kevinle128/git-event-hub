-- The state of the pull request is open, merged or closed. It is empty when
-- the hub has seen no lifecycle event of the pull request yet, and an empty
-- state counts as open. Existing rows get the empty state.
ALTER TABLE pr_index ADD COLUMN state TEXT NOT NULL DEFAULT '';
