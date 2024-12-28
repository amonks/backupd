-- name: CreateRemoteDataset :exec
update datasets
set remote_observed_at = unixepoch(),
    is_on_remote = true
where name = ?;

-- name: ObserveLocalDataset :one
insert into datasets (name, is_on_local, local_observed_at)
values (?, ?, ?)
on conflict (name) do update
  set is_on_local       = excluded.is_on_local,
      local_observed_at = excluded.local_observed_at
returning *;

-- name: RemoveOlderLocalDatasets :exec
update datasets
set local_observed_at = unixepoch(),
    is_on_local = false
where local_observed_at < ?;

-- name: ObserveRemoteDataset :one
insert into datasets (name, is_on_remote, remote_observed_at)
values (?, ?, ?)
on conflict (name) do update
  set is_on_remote       = excluded.is_on_remote,
      remote_observed_at = excluded.remote_observed_at
returning *;

-- name: RemoveOlderRemoteDatasets :exec
update datasets
set remote_observed_at = unixepoch(),
    is_on_remote = false
where remote_observed_at < ?;

-- name: GetLocalDatasets :many
select name from datasets where is_on_local = true;

-- name: GetRemoteDatasets :many
select name from datasets where is_on_remote = true;

