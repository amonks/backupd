pragma journal_mode='wal';

create table if not exists datasets (
  name         text primary key,

  is_on_local        boolean,
  local_observed_at  integer,

  is_on_remote       boolean,
  remote_observed_at integer
);

create table if not exists snapshots (
  dataset text not null references datasets(name),
  name    text not null,

  created_at integer not null,

  is_on_local        boolean,
  local_observed_at  integer,

  is_on_remote       boolean,
  remote_observed_at integer,

  primary key (dataset, name)
);

