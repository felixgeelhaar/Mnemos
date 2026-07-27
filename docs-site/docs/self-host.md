# Self-host

Single Go binary. Pick a backend, mint a JWT secret, point it at a DB.

## Storage backends

Set `MNEMOS_DB_URL` to one of:

| Scheme | Notes |
|---|---|
| `sqlite://path/to/db` | Default; pure-Go (no CGO), FTS5 |
| `memory://name` | In-process, evaporates on restart — for demos |
| `postgres://user:pass@host:5432/db?sslmode=disable` | Also works with **CockroachDB**, **YugabyteDB**, **Neon**, **Crunchy Bridge**, **TimescaleDB**, **AlloyDB Omni** unchanged |
| `mysql://user:pass@host:3306/db` | Also works with **PlanetScale**, **TiDB**, **MariaDB**, **Vitess** |
| `libsql://localhost?mode=file&file=…` | Pure-Go, supports Turso remote URLs and local files |

## Docker

```bash
docker run -d --rm -p 7777:7777 \
  -e MNEMOS_DB_URL="postgres://mnemos:mnemos@host:5432/mnemos?sslmode=disable" \
  -e MNEMOS_JWT_SECRET=$(openssl rand -hex 32) \
  ghcr.io/klarlabs-studio/mnemos serve
```

## Auth

```bash
mnemos user create --name demo --email demo@example.com
mnemos token issue --user usr_... --ttl 24h
```

**Secure by default since v0.85.1: every `/v1/*` and `/internal/*` endpoint
requires `Authorization: Bearer <token>` — reads included.** A tokenless
`GET /v1/beliefs` is a `401`. Only `/health`, `/healthz`, `/`, `/app` and the
rate-limited `POST /v1/leads` are anonymous.

If you deliberately want the old browsable-registry behaviour, opt in:

- `serve --public-reads` (`MNEMOS_PUBLIC_READS`) — anonymous `GET`/`HEAD`/
  `OPTIONS` on the data API. Warns at boot; ignored under `--require-tenant`.
- `serve --metrics-public` (`MNEMOS_METRICS_PUBLIC`) — anonymous
  `GET /internal/metrics` for a scrape on a trusted network. Deliberately not
  covered by `--public-reads`.

`/internal/ready` (version + DB write probe) is authenticated with no opt-out.

The signing key comes from `MNEMOS_JWT_SECRET` (hex, ≥ 32 bytes) or
`MNEMOS_AUTH_DIR/jwt-secret`, auto-created `0600` on first boot. Set it
explicitly in production and plan a rotation cadence
(`MNEMOS_JWT_PREV_SECRET` bridges the rollover).

## Compose templates

Production-shape compose files live under [`deploy/`](https://github.com/klarlabs-studio/mnemos/tree/main/deploy):

- `deploy/playground/` — public sandbox, in-memory backend, nginx rate limit
- The benchmarks stack at `benchmarks/docker-compose.yml` is also a viable starting point

## What the binary does NOT do

- **No managed service.** Mnemos is the binary. There's no hosted version, no SOC2 reseller, no per-call meter. If you want one of those, pick a hosted competitor.
- **No bundled UI.** A small registry browser ships at `/app` (HTML at `web/index.html`; `/` is the marketing landing page); for a richer UI bring your own.
- **No automatic backup.** Use whatever your storage backend provides (`pg_dump`, `sqlite3 .backup`, etc).
