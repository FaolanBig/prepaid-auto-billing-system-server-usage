# Prepaid Auto Billing

A Docker Compose application for monitoring a Shelly Plug M Gen3, recording power measurements in InfluxDB, and allocating electricity charges from prepaid user balances. It includes a small administrator panel, audit history, and CSV exports.

## What it does

- Polls the Shelly local RPC endpoint (default every 60 seconds).
- Writes measured power and cumulative energy to InfluxDB.
- Stores energy deltas, user membership periods, balances, billing runs, wallet transactions, and audit records in PostgreSQL.
- Splits each measured energy interval equally among users who were members during that interval. If membership changes within a poll interval, the energy is allocated by the duration each membership overlaps; this assumes energy was consumed uniformly during that short interval.
- Calculates cost using the configurable fixed rate in euros per kWh, records each charge, and reduces each user's prepaid balance.
- Queues email to the user and administrator after every billing run while that user's balance is below zero. Failed email delivery is logged and retried.
- Lets the administrator add, remove, and reactivate users without deleting history, refill balances, configure the rate and billing schedule, and download CSV exports.

## Deployment on server1

Install Docker Engine and the Docker Compose plugin on the Debian VM. Copy the deployment files to server1, then create the private environment file:

```sh
cp .env.example .env
```

Edit `.env` before starting the services:

1. Set `APP_IMAGE_TAG` to a version tag that has been published to GHCR by the [Docker workflow](./.github/workflows/docker.yml). The example uses `v1.0.0`. Replace all `replace-me` values and generate secrets with `openssl rand -hex 32`.
2. Set `SHELLY_URL` to the Shelly's local RPC URL, for example `http://192.168.1.50/rpc/Switch.GetStatus?id=0`. The Shelly must be reachable from server1. If Shelly authentication is enabled, set both `SHELLY_USERNAME` and `SHELLY_PASSWORD`.
3. Configure `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM`, `ADMIN_EMAIL`, and, if required, `SMTP_USERNAME` and `SMTP_PASSWORD`. SMTP STARTTLS is required for delivery. The service will not start without SMTP and admin email settings because negative-balance notifications are a core billing requirement.
4. Choose an administrator password with at least 16 characters and a unique cookie secret with at least 32 characters.

Start the services:

```sh
docker compose pull app
docker compose up -d
docker compose ps
docker compose logs -f app
```

The application image is pulled from `ghcr.io/faolanbig/prepaid-auto-billing-system-server-usage` using the `APP_IMAGE_TAG` in `.env`; deployment does not build the application locally. The repository's Dockerfile is used by the GitHub Actions workflow to build and publish version-tagged images. To upgrade, set `APP_IMAGE_TAG` to another published version, then run `docker compose pull app` and `docker compose up -d`.

By default, Docker Compose reads `.env` from the current working directory. You can keep the environment file in a separate, private directory and specify its path with `--env-file`. Specify the Compose file with `-f` as well if you are running the commands from another directory. For example:

```sh
docker compose --env-file /etc/powerbilling/.env -f /opt/powerbilling/docker-compose.yml pull app
docker compose --env-file /etc/powerbilling/.env -f /opt/powerbilling/docker-compose.yml up -d
docker compose --env-file /etc/powerbilling/.env -f /opt/powerbilling/docker-compose.yml ps
docker compose --env-file /etc/powerbilling/.env -f /opt/powerbilling/docker-compose.yml logs -f app
```

Replace those example paths with the actual locations of your `.env` and `docker-compose.yml`. Use `--env-file` on each Compose command that needs the deployment variables.

The admin panel is available at `http://127.0.0.1:8080` on server1 with the default binding. To reach it remotely, use an SSH tunnel or configure a TLS reverse proxy. If you change `APP_BIND_ADDRESS` to expose the port on a network interface, put it behind HTTPS and set `COOKIE_SECURE=true`; do not expose the admin panel directly to the public internet over plain HTTP.

The Compose stack keeps PostgreSQL and InfluxDB ports private to the Compose network and persists their data in named volumes. Back up both database volumes as well as a PostgreSQL logical dump as part of normal server maintenance. Keep `.env` private; it contains passwords and API tokens.

## Initial configuration

1. Sign in with `ADMIN_USERNAME` and `ADMIN_PASSWORD`.
2. Open **Settings** and enter the electricity price in euros per kWh. For example, enter `0.350000` for €0.35/kWh.
3. Configure the billing interval, local clock time, and IANA timezone. Defaults are 24 hours, `00:00`, and `Europe/Berlin`. A 24-hour interval runs daily at the configured local time. Other intervals repeat by their hour length, anchored to that clock time.
4. Add users and their initial prepaid contingents under **Users**.
5. Check the dashboard for the latest Shelly sample and next billing run. Check `docker compose logs app` if the device or SMTP server cannot be reached.

The first run is scheduled for the next matching occurrence of the configured local clock time after the rate is saved. Changing the interval, clock time, or timezone recalculates the next run; changing only the rate does not shift the schedule. If the tariff is configured after measurements have already been collected, intervals without a saved tariff use the tariff in effect when they are billed. Once set, each new interval snapshots the current tariff, so later tariff changes do not reprice existing intervals.

## Data and billing details

- The first valid Shelly reading establishes the cumulative-energy baseline; it is not billed. Subsequent positive energy-counter differences become energy intervals. If the counter decreases (for example, after a device reset), the app resets the baseline and does not create a negative charge.
- InfluxDB stores `shelly_power` points with `power_w` and `energy_total_wh` fields. PostgreSQL stores the interval ledger used for billing and exports.
- Removed users are deactivated, not deleted. Reactivating one starts a new membership period; it does not erase the gap while they were inactive, change their balance, or remove their history.
- If a reactivated user has a negative balance, that balance remains negative and they continue to receive notifications on billing runs until it is refilled.
- Billing uses the selected IANA timezone. Intervals that are whole-day multiples preserve the configured local clock across daylight-saving changes; the elapsed duration can therefore be 23 or 25 hours on a transition day. Other intervals advance by elapsed hours from the configured clock anchor.
- Energy measured when no user membership is active is recorded in total meter consumption but is not charged to a user.
- Rates are entered in euros per kWh to six decimal places. User charge amounts are rounded to cents; rounding remainders are assigned deterministically so the sum of user charges equals the rounded cost of the allocated energy.
- Dates shown and accepted by the export form are UTC. Both endpoints in an export date range are inclusive.
- Usage exports include per-user energy allocations from completed billing runs. Consumption exports include meter energy intervals, including intervals not billed yet. Billing exports include top-ups and charges.
- The default export types are **consumption**, **usage**, and **billing**. CSV exports can optionally be filtered to one user for usage or billing.
- Negative-balance email delivery is at-least-once: a process restart after an email is accepted but before it is marked sent can cause a duplicate. Delivery attempts and errors remain in PostgreSQL.

## Environment settings

See [.env.example](./.env.example) for the full list. Important settings:

| Variable | Purpose |
|---|---|
| `SHELLY_URL` | Shelly `Switch.GetStatus` local RPC URL |
| `SHELLY_POLL_SECONDS` | Poll interval from 5 to 3600 seconds; default 60 |
| `APP_IMAGE_TAG` | Published GHCR image tag to deploy |
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | Initial administrator login |
| `COOKIE_SECRET` | HMAC key for signed admin session cookies |
| `COOKIE_SECURE` | Set to `true` when the panel is served over HTTPS |
| `SMTP_*`, `ADMIN_EMAIL` | Outbound email settings and administrator recipient |
| `APP_BIND_ADDRESS`, `APP_PORT` | Host interface and port for the admin panel |

The application applies its idempotent PostgreSQL schema at startup. Do not remove the named volumes to upgrade or reset the application: deleting them destroys meter and billing history.
