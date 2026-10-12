-- Weekly meeting slots per section, for the /search table (days, start, end).
--
-- Stored as JSONB on the section rather than in a child table: a section has
-- one to a handful of slots, they are always read together with the section,
-- and they are rewritten wholesale by every poll. Shape (see source.Meeting):
--   [{"d":"YNYNYNN","s":"08:30","e":"09:20"}]   d = Mon..Sun flags, 24-hour times
-- An empty array means no scheduled time (online or TBA).
ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS meetings JSONB NOT NULL DEFAULT '[]'::jsonb;
