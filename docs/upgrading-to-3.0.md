# Upgrading to 3.0

3.0 removes the old Eraser names that [stability.md](stability.md) kept working since the rename in 2.3. If you already use the `delistd` command, the `DELISTD_*` variables and the `ghcr.io/drumandbytes/delistd` image, there is nothing to do. Your data is untouched: `~/.eraser`, the `eraser-data` volume and the encrypted mail passwords all keep working as they are.

## What was removed

| Removed | Use instead | What to do |
|---|---|---|
| The `eraser` command (the alias shipped beside `delistd`) | `delistd` | Change scripts, shell aliases and cron entries. The OS schedule: see below |
| `ERASER_*` environment variables (`ERASER_CONTAINER`, `ERASER_MACHINE_ID_FILE`, `ERASER_ALLOWED_HOSTS`, ...) | The same name with `DELISTD_` | Rename them in compose files, systemd units and Kubernetes manifests. They are no longer read at all, so a leftover `ERASER_*` setting silently does nothing |
| The `ghcr.io/drumandbytes/eraser` image tag | `ghcr.io/drumandbytes/delistd` | Change `image:` in compose and Kubernetes. The old tag stays at its last release (2.x) and gets no more updates |
| The rename notices (the "Eraser is now Delistd" card and the CLI line) | none | Nothing |
| The automatic move of a pre-rename OS schedule from `eraser` to `delistd` | `delistd schedule install` | See below |

## The OS schedule

An OS schedule (launchd on macOS, systemd on Linux) installed before 2.3 runs the `eraser` command. Releases 2.4 to 2.x moved it over to `delistd` the first time any command ran. 3.0 no longer does that, and the `eraser` binary is gone, so a schedule that was never migrated stops working. If you went straight from a release before 2.4 to 3.0, run this once:

```bash
delistd schedule install
```

It rewrites the schedule to run `delistd`. Check it with `delistd schedule status`.

## Docker

The image sets `DELISTD_CONTAINER` and `DELISTD_MACHINE_ID_FILE` itself. If your compose file or manifest overrode the old `ERASER_*` names, rename them. The volume is still called `eraser-data` and the data directory is still `/home/nonroot/.eraser`: both stay, on purpose, so nothing is orphaned.

## Homebrew

`brew upgrade` installs only `delistd` now. The old `eraser` binary is removed from the cask.

## What did not change

The data directory `~/.eraser`, the `eraser-data` volume, the launchd label and systemd unit names, the session cookie names and the encryption key derivation. Renaming them would make existing configs and histories unreadable, so they stay. The `eraser.drumandbytes.dev` redirect to delistd.dev stays too.
