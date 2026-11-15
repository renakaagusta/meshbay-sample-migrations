-- Migration 0004 is deliberately broken (MESH-133). It first changes the
-- schema, then fails, and the whole transaction must be rolled back so the
-- release that is already serving keeps a schema it understands.
ALTER TABLE items
	ADD COLUMN tag_id integer REFERENCES tags (id);

-- `items` has no tag_note column, so Postgres raises 42703, the migration
-- transaction rolls back (including the column above) and pre_deploy exits 1.
ALTER TABLE items
	ADD CONSTRAINT items_tag_note_check CHECK (tag_note <> '');
