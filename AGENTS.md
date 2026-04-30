# S-UI Agent Guide

> This file is intended for AI coding agents. It summarizes the project architecture, conventions, and workflows so you can be productive without guessing.

## Project Overview

**S-UI** is an advanced web management panel built on top of [SagerNet/sing-box](https://github.com/SagerNet/sing-box). It provides a multi-user, multi-inbound/outbound proxy setup with traffic statistics, subscription links, and a Vue 3 frontend. The backend is written in Go and embeds the compiled SPA directly into the binary.

- **Repository**: `github.com/alireza0/s-ui`
- **License**: GPL V3
- **Default credentials**: `admin` / `admin` (change in production)
- **Disclaimer**: This project is for personal learning and communication; do not use it for illegal purposes or in production environments without proper hardening.

## Technology Stack

| Layer | Technology |
|-------|------------|
| Backend language | Go 1.25.7 |
| Proxy engine | sing-box v1.13.4 |
| Web framework | Gin v1.12.0 |
| Database | SQLite (GORM v1.31.1, CGO via `mattn/go-sqlite3`) |
| Sessions | `gin-contrib/sessions` cookie store |
| Cron | `robfig/cron/v3` |
| Frontend | Vue 3 + TypeScript + Vuetify 4 |
| Build tool (frontend) | Vite 8 |
| State management | Pinia 3 |
| Router | Vue Router 5 (HTML5 history mode) |
| i18n | Vue I18n 11 (en, fa, vi, zhHans, zhHant, ru) |
| Charts | Chart.js / vue-chartjs |
| Notifications | Notivue |

## Repository Structure & Module Organization

```
.
├── api/              # HTTP handlers (API v1 session-based, API v2 token-based)
├── app/              # Application bootstrap and lifecycle coordinator
├── cmd/              # CLI subcommands (admin, setting, uri, migrate)
├── config/           # Embedded version/name, env-var config helpers
├── core/             # sing-box integration (Box wrapper, registries, stats, health checks)
├── cronjob/          # Scheduled jobs (stats, deplete, check core, WAL checkpoint, etc.)
├── database/         # GORM SQLite init, auto-migration, backup/import/export
│   └── model/        # GORM models: Setting, Tls, User, Client, Stats, Inbound, Outbound, Endpoint, Service, etc.
├── frontend/         # Vue 3 SPA (git submodule)
├── logger/           # Leveled logging wrapper (`op/go-logging`) with in-memory ring buffer
├── middleware/       # Gin middleware (domain validation)
├── network/          # AutoHttpsListener / AutoHttpsConn wrappers
├── service/          # Business logic layer (config assembly, inbounds, outbounds, clients, stats, panel)
├── sub/              # Subscription server and link generators (link / JSON / Clash)
├── util/             # Shared helpers (base64, link generation, JSON conversions)
│   └── common/       # Error helpers, array utils, random strings
├── web/              # Panel static file server (embedded `web/html/` via `//go:embed`)
├── windows/          # Windows build scripts and service XML
├── .github/workflows/# CI/CD (release, docker, windows)
├── build.sh          # Full build script (frontend + Go binary)
├── runSUI.sh         # Dev convenience script (build + run with debug)
├── install.sh        # Production installation script for Linux/macOS
├── s-ui.sh           # Interactive management script (start/stop/logs/update/SSL)
├── s-ui.service      # systemd unit file
├── Dockerfile        # Multi-stage build (Node → Go → Alpine)
├── Dockerfile.frontend-artifact  # CI-optimized Docker build
├── docker-compose.yml# Simple compose with ports 2095/2096
└── entrypoint.sh     # Docker entrypoint (runs migrate if DB exists, then execs binary)
```

### Key Package Responsibilities

- `main.go` — Entry point. Instantiates `app.NewApp()`, calls `Init()` then `Start()`, and traps OS signals (`SIGHUP` for restart, `SIGTERM` for stop). Delegates to `cmd.ParseCmd()` when CLI arguments are present.
- `app/` — Coordinates initialization order: logger → database → settings → sing-box core → cron jobs → web/sub servers.
- `core/` — Wraps sing-box lifecycle, managers, protocol registries, connection tracking, outbound health checks, and a custom log factory.
- `service/` — Assembles the final sing-box JSON configuration, handles CRUD for inbounds/outbounds/endpoints/clients, and exposes system metrics.
- `api/` — `APIHandler` (session auth) and `APIv2Handler` (token auth).
- `sub/` — Separate Gin engine serving subscription content on its own port/path.
- `web/` — Serves the embedded SPA. Injects `window.BASE_URL` into `index.html` for the Vue router. Falls back to `index.html` for unmatched routes (SPA history mode).

## Build & Development Commands

### Prerequisites

- **Go** 1.25 or later
- **Git** with submodule support
- **C compiler** (CGO is required: `gcc` on Linux, `musl-dev` on Alpine)
- **Node.js** (only if you modify the frontend; pre-built assets exist in `web/html/`)

### Quick Start (Backend-Only)

```bash
# Clone and init submodules
git clone https://github.com/alireza0/s-ui
cd s-ui
git submodule update --init --recursive

# Build frontend + backend and run in debug mode with a local DB
./runSUI.sh
```

`runSUI.sh` is equivalent to:

```bash
./build.sh
SUI_DB_FOLDER="db" SUI_DEBUG=true ./sui
```

The panel will be available at **http://localhost:2095/app/**.

### Manual Build

```bash
# 1. Build frontend (if needed)
cd frontend
npm install
npm run build
cd ..

# 2. Copy assets into the embedded folder
mkdir -p web/html
rm -fr web/html/*
cp -R frontend/dist/* web/html/

# 3. Build Go binary with required tags
BUILD_TAGS="with_quic,with_grpc,with_utls,with_acme,with_gvisor,with_naive_outbound,with_tailscale"
go build -ldflags '-w -s -checklinkname=0' -tags "$BUILD_TAGS" -o sui main.go
```

### Frontend-Only Development

```bash
cd frontend
npm install
npm run dev   # Vite dev server on port 3000, proxies /app/api → http://localhost:2095
```

### Docker

```bash
# Build locally
docker build -t s-ui .

# Or use compose
docker compose up -d
```

## Code Style & Conventions

### Go

- Follow **standard Go style** and **Effective Go**.
- Run `gofmt -w .` (or `goimports -w .`) before committing.
- Use **camelCase** for unexported names, **PascalCase** for exported names.
- Keep package names short and lowercase (`api`, `service`, `core`, `util`).
- Group imports: standard library, then third-party, then project imports.
- Use `common.NewError(...)` / `common.NewErrorf(...)` for wrapped errors (seen throughout the codebase).
- Use `json.RawMessage` for flexible config fields (inbounds, outbounds, endpoints, services) to avoid rigid structs when interfacing with sing-box.
- Handlers end with `Handler` (e.g., `APIHandler`).
- Services end with `Service` (e.g., `ConfigService`).

### Frontend

- Vue 3 Composition API with `<script setup>`.
- Use the `@/` alias for `src/` imports.
- Standard API response shape consumed via `HttpUtils`: `{ success: boolean, msg: string, obj: any }`.
- Vuetify defaults are configured for compact density and `solo-filled` inputs.
- Localization is synced between Vuetify locale and vue-i18n via `localStorage.getItem("locale")`.

## Testing Strategy

- **There is currently no formal test suite** in the repository (no `*_test.go` files).
- CI focuses on **build verification** rather than automated tests.
- Before submitting changes, ensure the project builds successfully:
  ```bash
  go build -ldflags "-w -s" -tags "with_quic,with_grpc,with_utls,with_acme,with_gvisor,with_tailscale" -o sui main.go
  ```
- Run `./runSUI.sh` and manually verify the affected area (panel page, API endpoint, subscription link, etc.).
- Contributions adding unit or integration tests (standard library `testing` package, table-driven style) are welcome.
- Optional linting: `go vet ./...`

## Configuration & Environment Variables

The backend reads configuration from **environment variables** and from the **SQLite `Setting` table**.

### Environment Variables

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `SUI_LOG_LEVEL` | `"debug" \| "info" \| "warn" \| "error"` | `"info"` | Log verbosity |
| `SUI_DEBUG` | `boolean` | `false` | Forces debug log level and Gin debug mode |
| `SUI_DB_FOLDER` | `string` | `"db"` (relative to binary) or `/usr/local/s-ui/db` | SQLite directory |
| `SUI_BIN_FOLDER` | `string` | `"bin"` | Directory for auxiliary binaries |
| `SINGBOX_API` | `string` | — | Optional string |

### Database Settings

Stored in the `Setting` model and editable via the web panel:

- Web panel port (default `2095`) and base path (default `/app/`)
- Subscription port (default `2096`) and base path (default `/sub/`)
- TLS certificate / key file paths
- Session max age, traffic record age, time location
- Subscription URI and options
- Raw sing-box configuration JSON

## Runtime Architecture

1. **Startup (`app/app.go`)**
   - Initialize logger.
   - Open SQLite database with WAL mode (`_journal_mode=WAL`), busy timeout 10s, max open connections 25.
   - Auto-migrate all GORM models.
   - Load settings into memory.
   - Create `core.Core` (sing-box wrapper).
   - Start cron jobs, web server, subscription server, then sing-box core.

2. **HTTP Servers**
   - **Web panel (`web/web.go`)** — Gin server on port 2095, path `/app/`. Serves embedded SPA, API v1 (`/api`), API v2 (`/apiv2`). Supports TLS via provided cert/key files. Session-based auth with `s-ui` cookie.
   - **Subscription server (`sub/sub.go`)** — Separate Gin server on port 2096, path `/sub/`. Serves link/JSON/Clash subscriptions. Also supports TLS.

3. **Cron Jobs (`cronjob/cronJob.go`)**
   - `StatsJob` — every 10s: collects traffic stats from sing-box and persists to DB.
   - `DepleteJob` — every 1m: disables expired or volume-exceeded clients and restarts affected inbounds.
   - `DelStatsJob` — daily (if `trafficAge > 0`): prunes old stats records.
   - `CheckCoreJob` — every 5s: ensures the sing-box core process is running.
   - `WALCheckpointJob` — every 10m: SQLite WAL checkpoint.

4. **Signal Handling (`main.go`)**
   - `SIGHUP` → graceful restart.
   - `SIGTERM` / `SIGINT` → graceful stop.

## Deployment & Packaging

### systemd (Linux)

`s-ui.service` is a `Type=simple` unit running `/usr/local/s-ui/sui` with `Restart=on-failure`.

### Install Script (`install.sh`)

- Detects OS/arch, downloads the release tarball from GitHub, extracts to `/usr/local/s-ui/`, copies `s-ui.sh` to `/usr/bin/s-ui`, installs service files, runs migration, and optionally configures the admin user.

### Management Script (`s-ui.sh`)

Interactive bash menu (and CLI shortcuts) for:
- start / stop / restart / status / logs / update / uninstall
- BBR toggle
- SSL certificate management (acme.sh, Cloudflare, self-signed)

### Docker / Compose

- **Image**: `alireza7/s-ui` (Docker Hub) and `ghcr.io/alireza0/s-ui`
- **Ports**: `2095` (panel), `2096` (subscription)
- **Volumes**: `./db` and `./cert`
- **Entrypoint**: `entrypoint.sh` runs `./sui migrate` if the database already exists, then `exec ./sui`.

### Windows

- Pre-built ZIPs are released for `amd64` and `arm64`.
- `install-windows.bat` installs the service (requires Administrator).
- `windows/` contains build scripts and an XML service configuration.

## CI/CD & Release Process

GitHub Actions workflows:

| Workflow | File | Triggers | Output |
|----------|------|----------|--------|
| **Release S-UI** | `release.yml` | Release published, push to `main`/tags, path changes | Static Linux binaries for 7 platforms (`amd64`, `arm64`, `armv7`, `armv6`, `armv5`, `386`, `s390x`) packaged as `s-ui-linux-<platform>.tar.gz` |
| **Build S-UI for Windows** | `windows.yml` | Release published, push to `main`/tags, path changes | Windows ZIPs (`amd64`, `arm64`) |
| **Docker Image CI** | `docker.yml` | Push tags, manual dispatch | Multi-arch Docker images (`linux/amd64`, `linux/386`, `linux/arm64/v8`, `linux/arm/v7`, `linux/arm/v6`) |

- Releases are marked **prerelease**.
- Dependabot scans `gomod` and `github-actions` ecosystems daily.
- Node.js 25 is used for frontend builds in CI.
- Linux release builds use static linking (`-static`) and Bootlin musl toolchains where applicable.

## Security Considerations

- **Default credentials**: The first login is `admin` / `admin`. The install script prompts to change this; otherwise change it immediately via the panel.
- **Session auth**: The web panel uses cookie-based sessions (`s-ui` cookie). API v2 uses token-based auth.
- **TLS**: The panel and subscription server can be secured with custom certificate/key files uploaded by the admin. File existence is checked before saving settings.
- **Domain validation**: A `DomainValidator` middleware exists for Gin.
- **Build tags**: Some protocols (QUIC, gRPC, uTLS, gVisor, Naive, Tailscale) are gated behind Go build tags; omitting them reduces attack surface but also removes features.
- **Database**: SQLite with WAL mode. Backups can be exported/imported via the panel.
- **No formal security audit** is mentioned; treat this as experimental software.

## Contributing & External Resources

- See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full development setup, coding conventions, and pull request process.
- API documentation lives in the [project wiki](https://github.com/alireza0/s-ui/wiki/API-Documentation).
- Issue templates exist for bug reports, feature requests, and questions.
- High-value contribution areas: multi-inbound-per-user UX, API completeness, subscription link conversions, tests, and documentation.
