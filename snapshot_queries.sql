-- name: ObserveLocalSnapshot :one
insert into snapshots (dataset, name, created_at, is_on_local, local_observed_at)
values (?, ?, ?, ?, unixepoch())
on conflict (dataset, name) do update
  set is_on_local       = excluded.is_on_local,
      local_observed_at = excluded.local_observed_at
returning *;

-- name: ObserveRemoteSnapshot :one
insert into snapshots (dataset, name, created_at, is_on_remote, remote_observed_at)
values (?, ?, ?, ?, unixepoch())
on conflict (dataset, name) do update
  set is_on_remote       = excluded.is_on_remote,
      remote_observed_at = excluded.remote_observed_at
returning *;

-- name: RemoveOlderLocalSnapshots :exec
update snapshots
set local_observed_at = unixepoch(),
    is_on_local = false
where dataset = ?
  and local_observed_at < ?;

-- name: RemoveOlderRemoteSnapshots :exec
update snapshots
set remote_observed_at = unixepoch(),
    is_on_remote = false
where dataset = ?
  and remote_observed_at < ?;

-- name: GetLocalSnapshots :many
select * from snapshots
where dataset = ?
  and is_on_local = true
order by created_at asc;

-- name: GetRemoteSnapshots :many
select * from snapshots
where dataset = ?
  and is_on_remote = true
order by created_at asc;

-- name: GetAllSnapshots :many
select * from snapshots
where dataset = ?
order by created_at asc;
