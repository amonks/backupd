-- name: ObserveLocalDataset :one
insert into datasets (name, is_on_local, local_observed_at)
values (?, ?, unixepoch())
on conflict (name) do update
  set is_on_local       = excluded.is_on_local,
      local_observed_at = excluded.local_observed_at
returning *;

-- name: RemoveOlderLocalDatasets :exec
update datasets
set local_observed_at = unixepoch(),
    is_on_local = false
where local_observed_at < ?;

-- name: RemoveOlderRemoteDatasets :exec
update datasets
set remote_observed_at = unixepoch(),
    is_on_remote = false
where remote_observed_at < ?;

-- name: ObserveRemoteDataset :one
insert into datasets (name, is_on_remote, remote_observed_at)
values (?, ?, unixepoch())
on conflict (name) do update
  set is_on_remote       = excluded.is_on_remote,
      remote_observed_at = excluded.remote_observed_at
returning *;

-- name: GetDataset :one
select * from datasets where name = ?;

-- name: GetLocalDatasets :many
select * from datasets where is_on_local = true;

-- name: GetRemoteDatasets :many
select * from datasets where is_on_remote = true;

-- name: GetAllDatasets :many
select * from datasets;
