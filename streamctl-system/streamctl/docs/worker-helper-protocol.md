# Worker helper protocol (next phase)

## Goal

Replace streamctl's knowledge of remote `systemd-run`, job workspaces, result
files, and `journalctl` with a small helper installed on each GPU worker.
Streamctl remains the source of truth for queue order and worker lifecycle; the
helper owns execution details on one worker.

The first implementation should preserve today's invariant: **at most one GPU
job runs at a time**, with normalization jobs taking precedence over render
jobs.

## Boundary

Today `dispatchGPUQueueOnce` connects over SSH and directly:

- discovers `streamctl-gpu-*.service` units;
- launches normalization and conf-render commands through `systemd-run`;
- reads unit state and journals;
- creates and removes render workspaces and result files.

The helper should absorb those operations. Provider lifecycle (RunPod,
DigitalOcean, or an external host), queue persistence, retries, and job policy
stay in streamctl.

## Transport

Start with a versioned JSON CLI over SSH rather than opening a network port:

```text
ssh <worker> streamctl-worker <command> --json
```

This keeps the current authentication and connectivity model, is easy to
bootstrap in cloud-init, and gives streamctl a stable typed contract. A later
HTTP transport can implement the same operations without changing queue logic.

Every response is one JSON object. Unknown fields must be ignored. Commands
return a non-zero exit status only when the operation itself could not be
performed; a failed job is represented by job state, not by a failing `status`
command.

## Version 1 operations

### `capabilities`

Used for readiness and compatibility checks.

```json
{
  "protocol_version": 1,
  "worker_id": "pod-or-host-identity",
  "job_kinds": ["normalize", "render"],
  "max_concurrency": 1,
  "tools": {"ffmpeg": "7.1", "conf_render": "0.1.0"}
}
```

### `submit`

Reads a request object from stdin. `request_id` is generated and persisted by
streamctl and makes submission idempotent.

Normalization payload:

```json
{
  "protocol_version": 1,
  "request_id": "normalize:<queue-id>:<attempt>",
  "kind": "normalize",
  "payload": {"source": "conference/recordings/raw.mp4"}
}
```

Render payload:

```json
{
  "protocol_version": 1,
  "request_id": "render:<job-id>:<attempt>",
  "kind": "render",
  "payload": {
    "manifest": {"version": 1, "jobs": []},
    "output_dir": "/root/streamctl-render-output/job-42"
  }
}
```

Manifest media paths are bucket object keys such as
`dev26/recordings/edits/keynote.mp4`. The worker stages those objects locally
and uploads completed artifacts under
`<conference>/recordings/renders/<streamctl-job-id>/`, writing `ready.json`
last.

Response:

```json
{"protocol_version":1,"job_id":"01J...","request_id":"render:42:1","state":"accepted"}
```

Submitting the same `request_id` again returns the original `job_id` and
current state.

### `list`

Returns active jobs and a bounded recent history. This replaces unit discovery
and allows streamctl to reconcile state after either process restarts.

```json
{
  "protocol_version": 1,
  "jobs": [{
    "job_id": "01J...",
    "request_id": "render:42:1",
    "kind": "render",
    "state": "running",
    "submitted_at": "2026-08-04T23:00:00Z",
    "started_at": "2026-08-04T23:00:01Z"
  }]
}
```

States are `accepted`, `running`, `succeeded`, `failed`, and `cancelled`.
Terminal records include `finished_at`, `exit_code`, and a short structured
error (`code` and `message`).

### `inspect <job-id>`

Returns one complete job record, including output metadata. For normalization,
output metadata includes the normalized object path. For render jobs it includes
the output directory and produced files.

### `logs <job-id>`

Returns UTF-8 log bytes. Version 1 may support `--tail <lines>` and
`--after <cursor>`; the response should include the next cursor so the UI can
poll without repeatedly transferring the entire journal.

### `cancel <job-id>`

Requests cancellation and is idempotent. Queue-level retry remains a streamctl
operation that creates a new attempt and therefore a new `request_id`.

## Streamctl adapter

Introduce a `WorkerClient` interface in Go so dispatch policy does not depend
on SSH or the helper process:

```go
type WorkerClient interface {
    Capabilities(context.Context) (Capabilities, error)
    Submit(context.Context, SubmitRequest) (Job, error)
    List(context.Context) ([]Job, error)
    Inspect(context.Context, string) (Job, error)
    Logs(context.Context, string, LogOptions) (LogChunk, error)
    Cancel(context.Context, string) error
}
```

Implement `SSHHelperClient` first. Keep the existing direct SSH/systemd path
behind a temporary legacy adapter until normalization and render reconciliation
tests pass against both implementations.

## Required correctness rules

1. Submission is idempotent by `request_id`.
2. Streamctl persists the helper `job_id` before considering an item running.
3. A helper restart reconstructs active and terminal job state from durable
   local metadata (systemd may remain its internal executor).
4. Paths are data, never shell fragments; the helper invokes child processes
   without a shell.
5. Manifest validation remains in streamctl and is repeated by conf-render.
6. Logs and error messages are bounded before storage or rendering.
7. Protocol/version incompatibility is shown as worker readiness failure and no
   job is dequeued.

## Suggested delivery sequence

1. Define protocol structs and golden JSON/compatibility tests in streamctl.
2. Implement helper `capabilities`, `list`, and `logs` around existing systemd
   units; switch worker diagnostics first.
3. Add idempotent normalization submission and reconciliation.
4. Add render submission, output metadata, and cleanup.
5. Add cancellation and cursor-based logs.
6. Remove direct remote `systemctl`, `journalctl`, workspace, and shell-command
   construction from streamctl after migration coverage is complete.
