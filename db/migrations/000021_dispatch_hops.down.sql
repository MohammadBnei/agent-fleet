DROP INDEX dispatch_decisions_agent_wakes_idx;
ALTER TABLE dispatch_decisions DROP COLUMN by_agent, DROP COLUMN hops, DROP COLUMN thread;
