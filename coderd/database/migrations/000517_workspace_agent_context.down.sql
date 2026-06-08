DROP TABLE IF EXISTS workspace_agent_context_resources;
DROP TABLE IF EXISTS workspace_agent_context_snapshots;

-- Enum values added to api_key_scope cannot be removed in Postgres without
-- recreating the type; this down migration is a no-op placeholder for those
-- additions and only drops the new tables.
