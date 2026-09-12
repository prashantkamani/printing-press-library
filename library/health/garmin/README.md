# Garmin Connect CLI

**Your whole Garmin history in a local database, not one 28-day page at a time.**

Garmin Connect caps every daily-stats request at 28 days and offers no personal API token, which is why the ecosystem is a handful of Python libraries rather than a tool you can call. This CLI signs in through your own browser, keeps each account's tokens in its own home, walks the whole history into SQLite, and answers sleep, training, heart-rate and activity questions from the archive with JSON on stdout.

Created by [@prashantkamani](https://github.com/prashantkamani) (Aria Kamani).

## Install

The recommended path installs both the `garmin-pp-cli` binary and the `pp-garmin` agent skill (Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot, and other agents supported by the upstream [`skills`](https://github.com/vercel-labs/skills) CLI) in one shot:

```bash
npx -y @mvanhorn/printing-press-library install garmin
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press-library install garmin --cli-only
```

For skill only — installs the skill into the same agents as the default command above, but skips the CLI binary (use this to update or reinstall just the skill):

```bash
npx -y @mvanhorn/printing-press-library install garmin --skill-only
```

To constrain the skill install to one or more specific agents (repeatable — agent names match the [`skills`](https://github.com/vercel-labs/skills) CLI):

```bash
npx -y @mvanhorn/printing-press-library install garmin --agent claude-code
npx -y @mvanhorn/printing-press-library install garmin --agent claude-code --agent codex
```

### Without Node (Go fallback)

If `npx` isn't available (no Node, offline), install the CLI directly via Go (requires Go 1.26.6 or newer):

```bash
go install github.com/mvanhorn/printing-press-library/library/health/garmin/cmd/garmin-pp-cli@latest
```

This installs the CLI only — no skill.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/garmin-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

Install the CLI binary first. The installer writes binaries to a per-user managed bin directory by default: `$HOME/.local/bin` on macOS/Linux and `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows.

```bash
npx -y @mvanhorn/printing-press-library install garmin --cli-only
```

Then install the focused Hermes skill.

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-garmin --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-garmin --force
```

Restart the Hermes session or gateway if the newly installed skill is not visible immediately.

## Install for OpenClaw
Install both the CLI binary and the focused OpenClaw skill. The installer defaults binaries to a per-user bin directory (`$HOME/.local/bin` on macOS/Linux, `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows):

```bash
npx -y @mvanhorn/printing-press-library install garmin --agent openclaw
```

Restart the OpenClaw session or gateway if the newly installed skill is not visible immediately.

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/garmin-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.
3. Leave the credential prompt blank and close it: Garmin issues no personal API key, and this bundle has none to paste. Connect the account once from a terminal with `garmin-pp-cli auth login --email <your Garmin account email>`; the MCP server reads the same home the CLI writes.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


```bash
go install github.com/mvanhorn/printing-press-library/library/health/garmin/cmd/garmin-pp-mcp@latest
```

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`). No credential belongs in this block: run `garmin-pp-cli auth login --email <your Garmin account email>` once, and the server picks the tokens up from the resolved home. Add `"env": {"GARMIN_HOME": "/path/to/that/home"}` only if you keep more than one Garmin account on this machine.

```json
{
  "mcpServers": {
    "garmin": {
      "command": "garmin-pp-mcp"
    }
  }
}
```

</details>

## Authentication

Garmin issues no personal API key. Sign-in goes through Garmin's own page in your browser: `auth login` opens it, catches the redirect back to a loopback port on 127.0.0.1, exchanges the ticket for an access token and a refresh token, and checks the account email on the resulting token against the one you named before writing anything to disk. The password never reaches this CLI, and the refresh token keeps the session alive without another browser visit. A token supplied through GARMIN_ACCESS_TOKEN or GARMIN_TOKEN is used as-is instead: it is never refreshed, it is not this home's stored chain, and plain `auth status` reports it as not this home's asserted account (run `auth status --verify` to have Garmin confirm which account that token belongs to). One Garmin account per home. To connect a second household account, in this order: sign out of Garmin in the browser; run the login under that account's own home with `GARMIN_HOME=~/.local/share/garmin-homes/other garmin-pp-cli auth login --email other@example.com`; confirm the account email the login prints is the one you meant; sign out of Garmin in the browser again. Clearing browser cookies is never required, and this CLI never attempts it — those sign-outs are ordinary sign-outs on Garmin's own site, so the choreography works for a user with no file-system access to the browser profile. Check which account a home holds with `garmin-pp-cli auth status` (add `--verify` to confirm it with one call to Garmin), and clear a home's stored chain with `garmin-pp-cli auth logout`.

## Quick Start

```bash
# One browser sign-in; the email you pass is checked against the account that actually signed in.
garmin-pp-cli auth login --email you@example.com

# Confirms the token works and prints the displayName the rest of the commands need.
garmin-pp-cli account social-profile

# Walks the date-ranged daily-stats series backwards into the local archive; the first run backfills, later runs resume.
garmin-pp-cli history --backfill

# Refreshes the activities feed and its per-activity detail; it does not walk the windowed daily-stats series, so history still owns that job.
garmin-pp-cli sync

# A month of nightly sleep summaries straight from Garmin.
garmin-pp-cli sleep stats --start 2026-08-01 --end 2026-08-28

```

## Unique Features

These capabilities aren't available in any other tool for this API.

### Auth you can trust with a household
- **`auth login`** — Signs in through Garmin's own page in your browser, catches the redirect on a loopback port, and refuses to store a token whose account email does not match the one you named.

  _Use this once per Garmin account per home; after it, everything else works unattended from the refresh token._

  ```bash
  garmin-pp-cli auth login --email you@example.com
  ```

### Local state that compounds
- **`history`** — Walks the date-ranged daily-stats series backwards into a local SQLite archive and resumes where it stopped — 28-day windows where Garmin caps a request at 28 days, 364-day windows where it does not, and one request per day for the per-day series.

  _Run this before any trend question; every analytics command reads the archive, not the API._

  ```bash
  garmin-pp-cli history --backfill
  ```

## Recipes

### Four weeks of sleep score in one call

```bash
garmin-pp-cli sleep score-stats --start 2026-08-10 --end 2026-09-06
```

The score trend is server-aggregated, so a month costs one request instead of twenty-eight per-night calls.

### What this account actually does

```bash
garmin-pp-cli activities breakdown --aggregation lifetime --metric duration
```

Lifetime totals grouped by activity type, computed by Garmin, without paging the activity list.

### Time in zone for the last ride

```bash
garmin-pp-cli activities list --limit 1 --json | jq -r '.[0].activityId' | xargs garmin-pp-cli activities hr-time-in-zones
```

Zone seconds for one activity; pair it with `heart-rate zones` to label the zones.

## Usage

Run `garmin-pp-cli --help` for the full command reference and flag list.

## Paths & environment variables

This CLI separates local files into four path kinds:

| Kind | Contents |
|------|----------|
| `config` | User-editable settings such as `config.toml` and saved profiles |
| `data` | Durable local data: `credentials.toml`, `data.db`, cookies, browser-session proof files, and other auth sidecars |
| `state` | Runtime state such as persisted queries, jobs, and `teach.log` |
| `cache` | Regenerable HTTP/cache files |

Each kind resolves independently. The ladder is:

1. Per-kind env var: `GARMIN_CONFIG_DIR`, `GARMIN_DATA_DIR`, `GARMIN_STATE_DIR`, or `GARMIN_CACHE_DIR`
2. `--home <dir>` for this invocation
3. `GARMIN_HOME` for a flat relocated root
4. XDG env vars: `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`
5. Platform defaults matching existing installs

For containers and agent sandboxes, prefer a single relocated root:

```bash
export GARMIN_HOME=/srv/garmin
garmin-pp-cli doctor
```

Under `GARMIN_HOME=/srv/garmin`, the four dirs resolve to `/srv/garmin/config`, `/srv/garmin/data`, `/srv/garmin/state`, and `/srv/garmin/cache`.

MCP servers do not receive CLI flags from the host. Put relocation in the host `env` block:

```json
{
  "mcpServers": {
    "garmin": {
      "command": "garmin-pp-mcp",
      "env": {
        "GARMIN_HOME": "/srv/garmin"
      }
    }
  }
}
```

Precedence matters in fleets: an ambient per-kind variable such as `GARMIN_DATA_DIR` overrides an explicit `--home` for that kind. Use `GARMIN_HOME` or the per-kind variables for durable fleet relocation; treat `--home` as the weaker per-invocation lever.

Relocation is one-way. Unsetting `GARMIN_HOME` does not move files back to platform defaults, and `doctor` cannot find credentials left under a former root. Move the files manually before unsetting relocation variables.

Existing installs keep working because the platform-default rung matches the legacy layout. On the first auth write, stored secrets leave `config.toml` and are consolidated into `credentials.toml` under the data directory. Run `garmin-pp-cli doctor --fail-on warn` to check path and credential-location warnings in automation.

## Commands

The novel `history` verb walks the date-ranged daily-stats series backwards into the local archive
and resumes where it stopped — 28-day windows where Garmin caps a request at 28 days (sleep stats,
sleep score, steps), 364-day windows where it does not (resting HR, VO2 max, intensity minutes), and
one request per day for the per-day series (daily summary, sleep detail, daily HR, training
readiness), bounded by `--days`. The generated `sync` verb covers the activities feed and its
per-activity detail (activity, HR zones, splits) — the flat resources the press can enumerate — and
cannot walk those windows itself. `history` is the command that keeps the archive current; run
`sync` when you want the generated activity detail refreshed on its own.

### account

Bootstrap and identity: social profile, unit settings, and the account email a login is checked against.

- **`garmin-pp-cli account personal-information`** - Returns the identity block for the signed-in account, including the account email. That email is
the only field on any Garmin response that names which account a token belongs to, so `auth
login` reads it immediately after the token exchange and refuses to store a token whose account
does not match the one the operator named. Run it before trusting a home that a shared browser
session may have filled with the wrong household member.
- **`garmin-pp-cli account settings`** - Returns the account's settings. Read this once per home to learn whether the account reports
distances in metric or statute units before formatting any distance, pace or weight.
- **`garmin-pp-cli account social-profile`** - Returns the account's public profile. The `displayName` field is the account key that several
other Garmin paths interpolate, so this is the first call after a login and the cheapest way to
confirm a stored token still works: a 401 or 403 here means the token was rejected by the data
tier.

### activities

The activity feed, per-activity detail and splits, lifetime breakdowns, and original file downloads.

- **`garmin-pp-cli activities breakdown`** - Server-side aggregation of every activity on the account into totals per parent activity type,
by duration or distance. Answers 'what does this account actually do, and how much' in one
request instead of paging the whole activity list.
- **`garmin-pp-cli activities download-original`** - Fetches the original recorded file for one activity as a ZIP containing the device FIT file.
This is the sanctioned export route for raw per-second data that the JSON endpoints do not
expose. Binary response; write it to a file rather than parsing it.
- **`garmin-pp-cli activities get`** - Everything Garmin holds about a single activity at summary level: type, timing, distance,
elevation, aggregate heart rate and power. Take the id from the activity list.
- **`garmin-pp-cli activities hr-time-in-zones`** - Seconds spent in each configured heart-rate zone during one activity, which is the raw material
for a training-polarisation ratio across a season. Combine with the zone settings to label the
zones.
- **`garmin-pp-cli activities list`** - The account's activity feed, one row per recorded activity, newest first. Pages with `--offset`
and `--limit`; an empty page means the end of the account's history. Optional filters narrow by
date range, activity type and sort order. Distinct local start dates over this list are how
'days active' is counted — Garmin exposes no server-side count.
- **`garmin-pp-cli activities splits`** - The lap or split breakdown of a single activity, each with its own distance, duration and
averages. Present for activities the device or the user split.

### fitness

Fitness level over time: VO2 max, max-met values, and Garmin's fitness-age estimate.

- **`garmin-pp-cli fitness age`** - Garmin's fitness-age estimate and the components it was computed from for a single date. There
is no range form, so a fitness-age trend costs one request per day and is what the local archive
exists for.
- **`garmin-pp-cli fitness max-metrics`** - Fitness-level trend: VO2 max (generic and cycling) plus the max-met value Garmin derived, one
row per date that has a measurement. Days without a qualifying activity are absent rather than
zero. At most 28 calendar days per request.

### heart_rate

Configured heart-rate zones, daily heart-rate detail, and per-activity time in zone.

- **`garmin-pp-cli heart-rate daily`** - The intraday heart-rate series for a single date plus the resting, minimum and maximum values
Garmin derived from it. Sampled values only exist for dates a wearable was worn. This is the
account-scoped path variant.
- **`garmin-pp-cli heart-rate daily-alt`** - Identical payload to `daily`, reached on the wellness-service path that takes the date as a
query parameter and needs no displayName. Kept as a fallback and for use before the bootstrap
profile call has run.
- **`garmin-pp-cli heart-rate zones`** - The zone boundaries the account has configured, per sport. These are settings, not measurements:
read them to label time-in-zone numbers, and expect them to change only when the user edits them
or Garmin auto-detects a new threshold.

### sleep

Sleep score trends, server-aggregated nightly summaries, and full per-night detail.

- **`garmin-pp-cli sleep night`** - Everything Garmin recorded for a single night: stage minutes, sleep windows, restlessness and
the sleep-score breakdown. One request per night, so a trend question should use the range
commands instead. This is the account-scoped path variant; `night-alt` is the same payload
without the displayName in the path.
- **`garmin-pp-cli sleep night-alt`** - Identical payload to `night`, reached on the sleep-service path that takes the date as a query
parameter and needs no displayName. It works before the bootstrap profile call has run, and is
the fallback if the account-scoped path ever changes.
- **`garmin-pp-cli sleep score-stats`** - The sleep score trend: one row per calendar date with the score value and qualifier. Cheaper
than fetching per-night detail when the question is about a trend rather than one night. At most
28 calendar days per request.
- **`garmin-pp-cli sleep stats`** - One row per night between `start` and `end`, aggregated by Garmin. At most 28 calendar days per
request; `history` walks longer ranges backwards in 28-day windows into the local archive and
de-duplicates on the calendar date. The generated `sync` verb does not chunk this series. Rows
arrive under `individualStats`.

### steps

Daily and weekly step totals against the account's goal.

- **`garmin-pp-cli steps daily`** - One row per calendar date with total steps, the step goal and the distance walked. At most 28
calendar days per request; `history` walks longer ranges in 28-day windows into the local archive.
The generated `sync` verb does not chunk this series.
- **`garmin-pp-cli steps weekly`** - Weekly step buckets instead of daily ones, which covers about a year in a single request where
the daily form would take thirteen. Use it for long-horizon trend questions and fall back to the
daily form when a specific date matters.

### training

Training status over a window and the daily training-readiness score.

- **`garmin-pp-cli training readiness`** - Garmin's training-readiness score for a single date, with the sleep, recovery, HRV and
acute-load inputs it was built from. One request per day; a readiness trend comes from the local
archive, not from this endpoint.
- **`garmin-pp-cli training status`** - Training status (productive, maintaining, unproductive, detraining, …) with acute and chronic
load for the days ending on the given date. Returns a page of days, not a single day; treat the
end date as the anchor.

### wellness

Daily wellness roll-ups: the day summary, intensity minutes, resting heart rate, and hydration.

- **`garmin-pp-cli wellness daily-summary`** - The single-day roll-up: steps, floors, intensity minutes, calories, resting heart rate and
stress. Check `includesWellnessData` before reading any wellness field — an account with no
wearable returns the envelope with the series absent, not zeroes.
- **`garmin-pp-cli wellness hydration`** - Water intake logged for a single date against the day's goal. Only populated for accounts where
the user logs hydration by hand or from a paired bottle.
- **`garmin-pp-cli wellness hydration-alt`** - The superset form of the hydration payload: everything the shorter path returns plus the
activity sweat-loss and goal-adjustment fields. Prefer this variant unless the extra fields are
unwanted.
- **`garmin-pp-cli wellness intensity-minutes-weekly`** - Weekly buckets of moderate and vigorous intensity minutes against the account's weekly goal.
This is the series behind Garmin's 'intensity minutes' badge.
- **`garmin-pp-cli wellness metrics-daily`** - A single named metric as a daily time series over a date range, selected by the numeric metric
id. Metric 60 is resting heart rate. Longer ranges are accepted here than on the 28-day stats
endpoints, but the exact ceiling is not published.


### Self-learning loop

This CLI caches per-question discovery so repeat queries skip the walk and structurally similar queries get answered via entity substitution. The loop also self-captures: every invocation is journaled locally, and failed-flag corrections plus fresh teaches surface as candidates on the next `recall` for confirm/reject judgment. Agents call `recall` before discovery and fire `teach &` after answering. See the `## Automatic learning` section in `SKILL.md` for the full protocol.

- **`garmin-pp-cli recall <query>`** - Look up cached resources for a query before running discovery
- **`garmin-pp-cli teach`** - Record a query -> resource mapping (silent on success, safe to background with `&`)
- **`garmin-pp-cli learnings list`** - Inspect taught rows
- **`garmin-pp-cli learnings forget <query>`** - Undo a teach
- **`garmin-pp-cli learnings candidates`** - List auto-captured candidates awaiting confirm/reject
- **`garmin-pp-cli learnings stats`** - Local loop metrics: recall hit rate, teach-to-reuse, playbook resolution, candidate counts
- **`garmin-pp-cli teach-pattern`** - Install a query/resource template up front
- **`garmin-pp-cli teach-lookup`** - Add an entity mapping (e.g. country code, team alias) for pattern substitution

Pass `--no-learn` or set `GARMIN_NO_LEARN=true` to disable the loop for deterministic flows.

The local store's schema version stamp is one-way: once this version of `garmin-pp-cli` opens the database, older binaries refuse it with a version error — upgrade the binary rather than downgrading.

## Output Formats

```bash
# Human-readable table (default in terminal, JSON when piped)
garmin-pp-cli activities list

# JSON for scripting and agents
garmin-pp-cli activities list --json
# Filter to specific fields
garmin-pp-cli activities list --json --select activityId,activityName,activityType

# Dry run — show the request without sending
garmin-pp-cli activities list --dry-run

# Agent mode — JSON + compact + no prompts in one flag
garmin-pp-cli activities list --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select <field>[,<field>...]` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **Read-only by default** - this CLI does not create, update, delete, publish, send, or mutate remote resources
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set

Exit codes: `0` success, `2` usage error, `3` not found, `4` auth error, `5` API error, `7` rate limited, `10` config error.

## Health Check

```bash
garmin-pp-cli doctor
```

Verifies configuration, credentials, and connectivity to the API.

## Configuration

Run `garmin-pp-cli doctor` to see the resolved config, data, state, and cache directories. The platform-default config path is `~/.config/garmin-pp-cli/config.toml`; `--home`, `GARMIN_HOME`, and per-kind env vars can relocate it.

Static request headers can be configured under `headers`; per-command header overrides take precedence.

Environment variables:

| Name | Kind | Required | Description |
| --- | --- | --- | --- |
| `GARMIN_ACCESS_TOKEN` | per_call | No | An access token obtained elsewhere. Garmin issues no personal API key, so there is normally nothing to put here — use `auth login`. A token set here is used as-is, is never refreshed, and is not the account `auth status` reports for this home. |
| `GARMIN_TOKEN` | per_call | No | Same as `GARMIN_ACCESS_TOKEN`. |
| `GARMIN_HOME` | path | No | Root directory for this account's config, data, state and cache. One Garmin account per home. |

### agentcookie (optional)

If you use agentcookie to sync secrets across machines, this CLI auto-adopts agentcookie-managed credentials with no extra setup. When the daemon writes to this CLI's config, `garmin-pp-cli doctor` reports `agentcookie: detected` and `auth-status` labels the source as `agentcookie`. Skip this section if you don't use agentcookie - the CLI works the same as any other.

## Troubleshooting
**Authentication errors (exit code 4)**
- Run `garmin-pp-cli doctor` to check credentials
- Run `garmin-pp-cli auth status` to see which home is resolved and which account it is bound to
- Run `garmin-pp-cli auth status --verify` to confirm the stored tokens still work; if they do not, run `garmin-pp-cli auth login --email <your Garmin account email>` again
**Not found errors (exit code 3)**
- Check the resource ID is correct
- Run the `list` command to see available items

### API-specific
- **`auth login` reports that the signed-in account does not match --email.** — Another Garmin session owned the browser. Sign out of Garmin in the browser, then run `auth login` again; nothing was written to your home.
- **Every wellness series comes back empty while activities are present.** — The account has no wearable paired. Bike computers and trainers record activities but no sleep, heart-rate or body-battery series; check `daily-summary` for `includesWellnessData` before assuming a bug.
- **A range longer than 28 days returns fewer rows than expected.** — Garmin caps daily-stats requests at 28 calendar days. Use `history` and query the local archive instead of asking one endpoint for a long range; `sync` alone does not walk this series.
- **Commands read the wrong person's data on a shared machine.** — Set GARMIN_HOME (or pass --home) per account; `doctor --json` prints which home directory each kind resolved to and why.

## Sources & Inspiration

This CLI was built by studying these projects and resources:

- [**tcgoetz/GarminDB**](https://github.com/tcgoetz/GarminDB) — Python (3292 stars)
- [**cyberjunky/python-garminconnect**](https://github.com/cyberjunky/python-garminconnect) — Python (2953 stars)
- [**matin/garth**](https://github.com/matin/garth) — Python (814 stars)
- [**bpauli/gccli**](https://github.com/bpauli/gccli) — Go (27 stars)

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
