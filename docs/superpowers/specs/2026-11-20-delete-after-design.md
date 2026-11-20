# Delete-after and VM history

## Intent

Short-lived VMs (probes, experiments) should clean themselves up: when creating a VM the user says how long it may live, and the hub deletes it once that time is up. Because VMs then disappear without anyone clicking "delete", deleted VMs must stay visible: what existed, who owned and created it, when it was created and deleted, and how long it lived.

## Goals

- Optional `delete-after` when creating a VM (API, CLI, UI), given as a duration (`30m`, `1h`, `2d`) or an ISO 8601 timestamp.
- The hub deletes the VM when the time is reached, through the normal delete path.
- The deletion time can be changed or removed later.
- A history of deleted VMs, viewable in the VMs page and the CLI, scoped like the VM list (mine / teams / all I may see).

## Non-goals

- Resource consumption (vCPU·h, RAM GiB·h, disk GiB·h) and costs. Later project; the history table is where it can grow.
- Backfilling VMs deleted before this release (from the audit log).
- Notifications before an expiry (there is no notification system).
- Delete-after on forks / snapshot restores at creation time; it can be set on the new VM afterwards.
- Deleting attached disks on expiry. Expiry behaves exactly like a manual delete: root disk and snapshots go, attached disks are released and kept (user's decision; revisit if leftover disks become a nuisance).

## Anti-goals

- A VM without delete-after is never deleted automatically.
- Deleted VMs cluttering the normal working list: the "All" filter keeps showing active VMs only.
- Touching every query on `vms` (soft delete) — the history lives in its own table.

## Approaches

- **A) Own history table `vm_history`, written by `DeleteVM`** — chosen. One new table, no change to existing `vms` queries, extensible for consumption later. Old deletions are not covered.
- B) Soft delete (`deleted_at` on `vms`) — rejected: 5 foreign keys, cascades on disks/attachments, `UNIQUE (org_id, name)`, node reconcile and mgmt IP allocation would all need `deleted_at IS NULL`; easy to miss one.
- C) Derive from the audit log (`vm.create` + `vm.delete`) — rejected: queries over JSON details, audit permissions instead of VM visibility, dead end for consumption.

Placement: the user chose to integrate history into the VMs page instead of a separate "Usage" page.

## Design

### Data

- `vms.delete_at timestamptz NULL` — absolute deletion time; NULL = never.
- New table `vm_history`: `id` (the VM's former id, PK), `org_id`, `user_id`/`team_id` (owner, one of them set), `name`, `cpus`, `memory_mb`, `disk_gb`, `image_ref`, `node_id`, `created_by`, `created_at`, `deleted_at`, `deleted_by` (NULL for automatic deletion), `reason` (`user` | `expired`).
  - Foreign keys to users/teams/nodes use `ON DELETE SET NULL` (or no FK) so history survives deleting a user, team or node; owner/creator names are resolved at read time and shown as "unknown" when gone. *(our call)*
- `DeleteVM` inserts the history row in the same transaction as the `DELETE`, taking actor and reason as parameters. Builder VMs (`kind = 'builder'`) get no history row. *(our call)*

### delete-after input

- Accepted forms: `<n>m`, `<n>h`, `<n>d` (minutes, hours, days; integers) or an RFC 3339 / ISO 8601 timestamp with offset. The hub turns it into an absolute `delete_at`.
- Validation: must be in the future, at least 5 minutes ahead *(our call, avoids a VM vanishing during boot)*; no upper bound.
- Parsed in one place on the hub; the CLI sends the raw string, the UI sends either the duration string or an ISO timestamp.

### API

- `POST …/vms` accepts optional `deleteAfter` (string). Invalid → 400.
- `PUT …/vms/{id}/delete-after` with `{"deleteAfter": "2d" | "<iso>" | null}` — sets or clears; requires `vm:delete` for every change (user's decision: an expiry is a delayed delete, so team members without delete rights must not set it; they cannot extend or clear it either). Audited with its own action name, following the existing naming *(our call)*. The UI hides the edit control without `vm:delete`.
- VM JSON gains `deleteAt` (ISO or null).
- `GET …/vms/history` returns deleted VMs the caller may see, newest deletion first. Visibility: a history row is shown if the caller is its owning user, a current member of its owning team, or `Allowed(VMObject(id), "vm:read")` passes (org-wide and custom-role rules). Per-VM grants vanish with the VM, so membership is checked against the row. *(our call: plain team members see their team's deleted VMs)* No pagination in v1 *(our call; add when lists get long)*.

### Expiry loop

- A hub loop (pattern of `AutoRebuilds`, started in `cmd/hub/main.go`) runs every minute: selects VMs with `delete_at <= now()` and `kind = 'vm'`, deletes each via `DeleteVM` with reason `expired`, writes a `vm.delete` audit entry with `actor_kind = system` and `reason: expired`, drops metrics and notifies the fleet — the same side effects as the API delete.
- `ErrConflict` (a fork still copying from the VM) → skip, retry next tick. Other errors are logged; one failing VM doesn't stop the others.
- Works regardless of the node being online; the node reconciles the deletion when it reconnects (as for manual deletes).

### CLI

- `observatory vm create … --delete-after <dur|iso>`.
- `observatory vm delete-after <vm> <dur|iso|off>`.
- `observatory vm show` shows `delete at` (absolute + relative); `vm list` shows it in a column when any VM has one *(our call)*.
- `observatory vm list --deleted` prints the history: name, owner, size, created (when/by), deleted (when/by or `expired`), lifetime.

### UI

- `CreateVMDialog.vue`: optional "Delete after" field (text input accepting the same forms, with hint), empty = never.
- VM list and detail page show "deletes in 1 h 20 min" when set; detail page lets the user change/clear it.
- `VMsView.vue`: status segment gets **Deleted**. When selected, the page loads `GET …/vms/history` and shows a plain table instead of `VMList`: Name, Owner, Size, Created (when/by), Deleted (when/by or "expired"), Lifetime. Rows aren't clickable and have no actions. Owner tabs (All/Mine/Teams) and search apply as usual.

```
VMs                                                   [+ Create VM]
All  Mine  Teams   │ All │ Running │ Paused │ Hibernated │ Stopped │ Deleted │
NAME         OWNER      SIZE           CREATED               DELETED                 LIFETIME
auth-probe   C. Müller  1×4 GiB×100    18.11. 09:12 C.Müller 19.11. 09:12 expired    1d 0h
bold-wolf    C. Müller  1×4 GiB×100    17.11. 14:03 C.Müller 17.11. 16:40 C.Müller   2h 37m
```

### Docs

- `docs/vm-lifecycle.md` (delete-after, history) and `docs/cli.md` (new flags/commands).

## Left to the builder

- Loop interval constant and whether expiry candidates are selected with `FOR UPDATE SKIP LOCKED` (single hub today).
- Exact i18n strings, relative-time formatting, column widths.
- Migration numbering and down migration.
- Test layout (store tests for history insert and expiry selection, API tests for parsing/validation/visibility, CLI golden files).
