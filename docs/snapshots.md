# Automatic checkpoints

The time machine uses local, disk-only Incus snapshots. It is not an off-host
backup: reinstalling or deleting the container removes its checkpoints, and a
storage-pool failure can destroy them all. Creating a checkpoint does not run
application-consistency hooks or deliberately stop the container.

## Account quotas

Every account has its own persisted snapshot quota. Administrators set it in
the create/batch-create form or with `vps add USER --snapshots N`, and can edit it
later in the quota form or with `vps quota USER --snapshots N`. Zero gives the
account no snapshot positions; negative and fractional quotas are rejected.
Omitting the creation value uses the built-in default of one. There is no global
quota configuration. The admin browser remembers the last creation quota in
localStorage, including zero, and uses it for both single and batch creation.
This is a per-browser convenience, not a shared setting; unavailable storage
falls back to the normal form defaults. Editing an existing account does not
change that creation preference. Reinstall preserves the account's quota.
Transfer metadata carries an explicitly set quota, including zero; old transfer
archives without it use the built-in default of one. Snapshot history and
automatic schedules themselves do not travel.

Quota reductions must satisfy both:

- New quota >= the number of all existing fixed and automatic checkpoints.
- If automatic checkpoints are enabled, new quota >= fixed count + 1.

For example, with four fixed checkpoints and an enabled schedule that has not
created its first automatic point, the quota cannot drop from five to four:
the next automatic creation still needs its reserved position. With one fixed
and three automatic checkpoints, reducing five to four is safe, but three is
rejected. With scheduling disabled and no checkpoints, reducing to zero is safe.
Quota edits never delete checkpoints, convert them or disable the schedule.
Reductions require a successful authoritative Incus catalog read and are
serialized with checkpoint creation/policy changes. Increases and unchanged
quotas are allowed even when Incus is unavailable.

## User policy

Each container has an independent, opt-in schedule in the time-machine dialog.
The interval is an integer N >= 1 elapsed hours, not a wall-clock cron rule, so
there is no time-zone or daylight-saving ambiguity. The technical maximum is
2,562,047 hours (Go's duration limit).

- Enabling requires at least one currently unused position in the account's quota.
- Enabling immediately attempts to create the first automatic checkpoint, then
  subsequent attempts run every N hours. Changing N starts a new countdown
  without creating an extra checkpoint; saving an unchanged policy does not
  postpone the existing countdown.
- The scheduler checks every 30 seconds and executes at most one automatic
  operation at a time. Long-running lifecycle operations may delay execution.
- The deadline and automatic-snapshot intent are persisted before creation.
  Restarting `vps.service` retains policies and classification. Missed intervals
  are not replayed: an overdue policy gets one attempt, then waits N hours.
- A failed or skipped attempt also waits N hours; there is no tight retry loop.
  If the initial attempt fails, enabling returns an explicit error but the policy
  remains enabled with its failure recorded and a retry scheduled. Existing
  checkpoints are preserved.
- Expired or non-ready accounts, an account quota of zero, a storage pool at
  least 90% full, and unreadable pool/snapshot information prevent creation.
  A normally stopped, ready container can still receive disk-only checkpoints.
- `vps serve` runs the scheduler independently of resource sampling. Saved
  schedules continue even if both HTTP panel entrances are disabled. Nothing
  runs while `vps.service` is stopped.

## Quotas and classification

Let T be the account's stored total quota, F fixed checkpoints and A automatic ones.
When automatic snapshots are enabled, stable state requires F <= T - 1 and
F + A <= T. Automatic checkpoints can use every otherwise free position, not
just the one reserved position.

| Operation while enabled | Requirement / effect |
| --- | --- |
| Create a fixed checkpoint manually | Both F + A < T and F < T - 1; never deletes another checkpoint |
| Convert automatic to fixed | F < T - 1; changes classification only, not the count, disk data or creation time |
| Scheduled automatic creation | Create first; if over T, delete the oldest automatic checkpoint |
| Disable automatic snapshots | Stop scheduling and atomically classify all existing automatic checkpoints as fixed; delete no disk checkpoints |

When disabled, all checkpoints are fixed and manual creation only requires a
free total position. Re-enabling still requires at least one free position.
With T = 1, enabling reserves the only position for automatic checkpoints, so
manual creation and conversion are unavailable until the policy is disabled.

A full total quota rejects manual creation even if the fixed quota has room.
For example, T=5 / F=1 / A=4 permits conversion but not manual creation; T=5 /
F=4 / A=1 permits neither. F=4 / A=0 also rejects manual creation while enabled:
the remaining position is reserved for the next automatic checkpoint.

Fixed means exempt from automatic rotation, not immune to explicit destructive
actions. Both kinds can be restored or deleted. Only fixed checkpoints may be
shared. Sharing deletes all older checkpoints of either kind; restoring deletes
all newer checkpoints of either kind, on all storage drivers. These actions
also reconcile classification and share mappings with the actual Incus list.

Existing or externally created snapshots with no automatic metadata are fixed.
Names alone never make a checkpoint automatic. Ordinary reinstall keeps the
policy, clears the old classification and restarts the countdown. Migration
archives do not carry checkpoint history or automatic policies.

## Failure and concurrency

Only automatic rotation may temporarily exceed the count cap by one. New
creation must succeed before an old automatic checkpoint is deleted. A creation
failure leaves the old recovery points untouched. A cleanup failure is reported
as "created; rotation failed" rather than a creation failure.

An interrupted rotation may leave the container over quota. The next scheduled
attempt must repair that excess before creating anything else; failed repair
blocks new creation rather than growing without bound. Fixed checkpoints are
never selected for automatic cleanup. Normal administrator quota edits reject
values that would invalidate the fixed reservation. If external database/Incus
changes bypass these guards, the scheduler pauses rather than deleting fixed
history. Disabling during an over-quota failure preserves all data and can leave more
fixed checkpoints than the cap; the user must explicitly delete the excess.

Lifecycle operations, snapshot mutation and schedule changes share an in-process
mutex and a Linux flock sidecar next to the database (`.operations.lock`). The
sidecar is preserved during normal use and root-created files inherit database
ownership so `vps serve` remains unprivileged. This protects CLI export/import,
reinstall and deletion against the daemon's scheduler. It does not coordinate
external tools that bypass vpsmgr and call Incus directly. No SQLite transaction
is held across an Incus operation.

## Database upgrade

Schema version **20 -> 21** adds two tables and one index:

- `snapshot_schedules`: one row per user, storing enabled state, interval hours,
  next attempt, last attempt, last successful creation, and the last error.
- `automatic_snapshots`: `(user_id, snapshot)` primary key identifying automatic
  checkpoints, including durable pre-creation intents.
- `snapshot_schedules_due`: partial index for enabled policies ordered by next
  execution time and user ID.

Both tables reference `users(id)` with `ON DELETE CASCADE`. Schema **21 -> 22**
adds `users.snapshot_limit INTEGER NOT NULL DEFAULT 1 CHECK(snapshot_limit>=0)`.
Inside that migration's transaction, all existing users inherit the actual
configured pre-upgrade `snapshots.limit` value, including zero, exactly once.
They are not all assigned the hardcoded default of one. The retired
`snapshots.limit` YAML key remains readable solely for this upgrade; it is absent
from the configuration registry, CLI settings and newly serialized YAML.
Runtime database opens pass the loaded legacy value to
`db.OpenWithLegacySnapshotQuota`. That value never supplies a new account's quota,
and reopening a version-22 database never rewrites stored quotas. Share tables
are unchanged; retiring the configuration key requires no further schema change.

Saving configuration first migrates any existing database with the loaded legacy
quota before removing the old key, including when another setting is changed
with `--no-apply`. If the database path is changed, both the original and any
existing replacement database are migrated before retiring the source. If
migration fails, saving fails without replacing the old configuration. A missing database is not created just to save configuration.
Preserve the original YAML value until the first upgrade: manually removing or
changing it beforehand loses that migration input. Once migrated, existing
accounts depend only on their persisted quotas.

The normal transactional migration runs on database open; when upgrading from
version 20, all existing checkpoints remain fixed and no schedules are enabled.
Back up the database before deploying: older binaries reject the newer schema,
so downgrading requires the matching pre-upgrade database backup too.

## Tests

Ordinary Go tests use temporary databases and a fake Incus Unix-socket server,
covering quotas, conversion, disable/re-enable, deadlines, restart persistence,
rotation ordering/failures, lifecycle eligibility, sharing/restore cleanup,
per-account quota isolation, safe reductions and automatic reservation, catalog
failures, combined quota rollback, legacy quota migration and creation/edit forms.
They require neither root nor a live Incus daemon. When Node.js is installed, an
additional test executes the rendered preference JavaScript with a minimal DOM,
covering save/reload, zero, batch creation, old/corrupt/blocked localStorage and
isolation from existing-user quota edits; it needs no browser or npm packages.

There is also an explicit, destructive-to-its-own-fixture integration test for a
fully installed disposable host. Stop the previous panel first, back up its
binary/database, and run from `src`:

```sh
CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  VPSMGR_SNAPSHOT_INTEGRATION=1 \
  go test -p 1 ./internal/mgr -run '^TestSnapshotIntegrationIncus$' -v -timeout 15m
```

It creates one disposable account with an explicit quota of five, checks real
checkpoint creation/rotation, independent quotas, safe reductions, promotion,
disk restore and sharing, then removes the account. While the panel is stopped,
it temporarily enables the shared snapshot-sharing mirror and restores its
previous effective state afterwards; authorize this before running. It opens
and migrates the configured database using any legacy quota solely for existing
accounts; do not enable it on a production host.

After deploying the new binary and starting `vps.service`, a second opt-in test
connects to its real local HTTPS listener:

```sh
CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  VPSMGR_SNAPSHOT_HTTP=1 \
  go test -p 1 ./internal/mgr -run '^TestSnapshotIntegrationHTTP$' -v -timeout 10m
```

It creates its own disposable account with an explicit quota of one, checks the
bilingual user UI and schedule forms, advances only that fixture's saved deadline,
and waits for the service's actual background scheduler to create an automatic
checkpoint. It also checks disable conversion, reservation/count rejection,
safe reductions to zero, and the installed `/usr/local/bin/vps` quota command.
When the administrator entrance is enabled, it creates and removes a temporary
admin session to verify its bilingual quota UI and real quota-edit forms too.
The local test client accepts the panel's self-signed certificate. It logs no
session tokens, account passwords or secret panel paths. As with the Incus test,
use a disposable host and restore database/WAL/SHM ownership to the `vps` user
before starting the unprivileged service if root has created those files.
