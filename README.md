# Prepaid Auto Billing

A Docker Compose application for monitoring a Shelly Plug M Gen3, recording power measurements in InfluxDB, and allocating electricity charges from prepaid user balances. It includes a small administrator panel, audit history, and CSV exports.

## What it does

- Polls the Shelly local RPC endpoint (default every 60 seconds).
- Writes measured power and cumulative energy to InfluxDB.
- Stores energy deltas, user membership periods, balances, billing runs, wallet transactions, and audit records in PostgreSQL.
- Splits each measured energy interval equally among users who were members during that interval. If membership changes within a poll interval, the energy is allocated by the duration each membership overlaps; this assumes energy was consumed uniformly during that short interval.
- Calculates cost using the configurable fixed rate in euros per kWh, records each charge, and reduces each user's prepaid balance.
- Queues email to the user and administrator after every billing run while that user's balance is below zero. Failed email delivery is logged and retried.
- Lets the administrator add users, remove them without deleting history, refill balances, change the rate and billing interval, and download CSV exports.

## Deployment on server1

Install Docker Engine and the Docker Compose plugin on the Debian VM. Copy this repository to server1, then create the private environment file:

```sh
cp .env.example .env
```

Edit `.env` before starting the services:

1. Replace all `replace-me` values. Generate secrets with `openssl rand -hex 32`.
2. Set `SHELLY_URL` to the Shelly's local RPC URL, for example `http://192.168.1.50/rpc/Switch.GetStatus?id=0`. The Shelly must be reachable from server1. If Shelly authentication is enabled, set both `SHELLY_USERNAME` and `SHELLY_PASSWORD`.
3. Configure `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM`, `ADMIN_EMAIL`, and, if required, `SMTP_USERNAME` and `SMTP_PASSWORD`. SMTP STARTTLS is required for delivery. The service will not start without SMTP and admin email settings because negative-balance notifications are a core billing requirement.
4. Choose an administrator password with at least 16 characters and a unique cookie secret with at least 32 characters.

Start the services:

```sh
docker compose up -d --build
docker compose ps
docker compose logs -f app
```

The admin panel is available at `http://127.0.0.1:8080` on server1 with the default binding. To reach it remotely, use an SSH tunnel or configure a TLS reverse proxy. If you change `APP_BIND_ADDRESS` to expose the port on a network interface, put it behind HTTPS and set `COOKIE_SECURE=true`; do not expose the admin panel directly to the public internet over plain HTTP.

The Compose stack keeps PostgreSQL and InfluxDB ports private to the Compose network and persists their data in named volumes. Back up both database volumes as well as a PostgreSQL logical dump as part of normal server maintenance. Keep `.env` private; it contains passwords and API tokens.

## Initial configuration

1. Sign in with `ADMIN_USERNAME` and `ADMIN_PASSWORD`.
2. Open **Settings** and enter the electricity price in euros per kWh. For example, enter `0.350000` for €0.35/kWh.
3. Set the billing interval in hours. It defaults to 24 hours; setting 1 runs hourly. Saving a different interval schedules the next run from the time of the change.
4. Add users and their initial prepaid contingents under **Users**.
5. Check the dashboard for the latest Shelly sample and next billing run. Check `docker compose logs app` if the device or SMTP server cannot be reached.

Billing starts after the first complete configured interval. If the tariff is configured after measurements have already been collected, intervals without a saved tariff use the tariff in effect when they are billed. Once set, each new interval snapshots the current tariff, so later tariff changes do not reprice existing intervals.

## Data and billing details

- The first valid Shelly reading establishes the cumulative-energy baseline; it is not billed. Subsequent positive energy-counter differences become energy intervals. If the counter decreases (for example, after a device reset), the app resets the baseline and does not create a negative charge.
- InfluxDB stores `shelly_power` points with `power_w` and `energy_total_wh` fields. PostgreSQL stores the interval ledger used for billing and exports.
- Removed users are deactivated, not deleted. Their membership end time is retained and their balance/history remains available.
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
| `ADMIN_USERNAME`, `ADMIN_PASSWORD` | Initial administrator login |
| `COOKIE_SECRET` | HMAC key for signed admin session cookies |
| `COOKIE_SECURE` | Set to `true` when the panel is served over HTTPS |
| `SMTP_*`, `ADMIN_EMAIL` | Outbound email settings and administrator recipient |
| `APP_BIND_ADDRESS`, `APP_PORT` | Host interface and port for the admin panel |

The application applies its idempotent PostgreSQL schema at startup. Do not remove the named volumes to upgrade or reset the application: deleting them destroys meter and billing history.
