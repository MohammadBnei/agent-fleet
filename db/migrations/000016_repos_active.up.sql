-- Whether the fleet uses this repo. The GitHub installation decides what the
-- fleet can reach (docs/adr/0074); this is the console's one write on repos,
-- deciding what it uses. An inactive repo starts no new session; sessions it
-- already has keep working. Sync never names the column, so it survives every
-- tick. New repos default to active.
ALTER TABLE repos ADD COLUMN active BOOLEAN NOT NULL DEFAULT true;
