# Upgrading

An upgrade is normally just deploying the new release: the server applies
pending database migrations when it starts. This page says how releases are
published and what to check before you deploy one.
[Running in production](production.md#operating-it) covers the rollout
itself, rolling back, and backups.

## Releases

Each release has a version tag (`vX.Y.Z`), release notes on GitHub, and an
entry in [`CHANGELOG.md`](../CHANGELOG.md). It publishes:

- the server's container image, `ghcr.io/bluesky-social/diderot:vX.Y.Z`
  (`latest` is the newest release);
- the client download for people's machines, and its Homebrew formula
  ([The `diderot` command](cli.md#install)).

A change that needs you to do something is marked `BREAKING CHANGE` in the
release notes, with what to change. Read the notes of every release between
the one you run and the one you deploy.

## Database migrations

The server applies the migrations it has and the database lacks before it
serves anything, and logs `migrations applied`. There is no separate
migration step, and no migration is ever undone:

- Back up the database before a release that has a migration, if you may
  want to go back ([Backup and restore](production.md#backup-and-restore)).
- An older release starts against a newer schema without complaint, and
  whether it then works is not guaranteed. The same holds during a rolling
  update, while old replicas run against the migrated schema.

## Settings that moved or were removed

When a release removes a deployment variable, because its setting moved to
the admin UI or went away, there is no fallback and nothing is imported.
Every command that reads the server's configuration (`serve`, `reset-admin`,
and the other server commands) refuses to start while a removed variable is
set, and names each one and where its setting is now. A half-finished move
therefore fails at startup instead of running without the setting.

Before you deploy, take the variables the release notes list out of the
deployment, in the same change. If the setting moved to a page, note its
value first and enter it there after the upgrade; until you do, the built-in
default applies. [Removed](configuration.md#removed) lists the variables the
server refuses.

## Prompts

An install's prompts are never replaced by an upgrade. When a release ships
changed prompt text, the Prompts page says how many prompts differ from it,
and **Shipped text** takes the ones you choose into the draft
([Prompts](prompts.md#the-example)).

## The Slack app

A release that needs a new Slack permission or event says so in its notes.
Update your app from the current
[`slack-app-manifest.yaml`](../slack-app-manifest.yaml) as
[Changing an existing app](slack-app.md#changing-an-existing-app) describes,
then click **Test** on the Slack connector page: it reports a permission the
app lacks.

## The client

People upgrade the `diderot` on their machines with `brew upgrade diderot`,
or by installing the newer download, and run `diderot setup <url>` again to
bring the agent skill up to date ([Install](cli.md#install)).
