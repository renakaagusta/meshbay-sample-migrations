CREATE TABLE tags (
	id serial primary key,
	name text not null unique,
	created_at timestamptz not null default now()
);
