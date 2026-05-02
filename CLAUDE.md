# CLAUDE.md — http2mac

This file provides context for AI-assisted development on the `http2mac` project.

---

## Project overview

`http2mac` is a single-binary HTTP gateway that exposes IEEE OUI / MAC address lookup as a JSON REST API.
It is written entirely in Go and embeds all static assets (web UI, favicon, OpenAPI spec) at compile
time using `//go:embed` directives, so the resulting binary has zero runtime file dependencies.

The server accepts `POST /api/v1/mac` requests containing one or more MAC addresses and returns
structured OUI registration data. The database is **built automatically** from a gzipped CSV fetched from
the **letstool CDN** (`https://cdn.letstool.net/mac/csv`) and refreshed every 24 hours.
An optional `LICENSE_KEY` enables licensed (higher-quota) CDN access; the server works anonymously without one.

---

## Repository layout

```
.
├── api/
│   └── swagger.yaml              # OpenAPI 3.1 source (human-editable)
├── build/
│   └── Dockerfile                # Two-stage Docker build (builder + scratch runtime)
├── cmd/
│   └── http2mac/
│       ├── main.go               # Entire application — single file
│       └── static/
│           ├── favicon.png       # Embedded at build time
│           ├── index.html        # Embedded web UI (dark/light, 15 languages, RTL support)
│           └── openapi.json      # Embedded OpenAPI spec (generated from swagger.yaml)
├── scripts/
│   ├── 000_init.sh               # go mod tidy
│   ├── 999_test.sh               # Integration smoke tests (curl + jq)
│   ├── linux_build.sh            # Native static binary build
│   ├── linux_run.sh              # Run binary on Linux
│   ├── docker_build.sh           # Build Docker image
│   ├── docker_run.sh             # Run Docker container
│   ├── windows_build.cmd         # Native build on Windows
│   └── windows_run.cmd           # Run binary on Windows
├── go.mod
├── go.sum
├── LICENSE                       # Apache 2.0
├── README.md
└── CLAUDE.md                     # This file
```

---

## Key design decisions

- **Single `main.go`**: the entire server logic lives in `cmd/http2mac/main.go`. There are no internal packages.
- **Embedded assets**: `favicon.png`, `index.html`, and `openapi.json` are embedded with `//go:embed`. Any change to these files is picked up at the next `go build`.
- **Static binary**: the build uses `CGO_ENABLED=0` and `-ldflags "-extldflags -static"`. Do not introduce `cgo` dependencies.
- **No framework**: the HTTP layer uses only the standard library (`net/http`). Do not add a router or web framework.
- **IPv6 MMDB keyspace**: MAC address blocks are stored in a custom MaxMind-compatible MMDB using a synthetic ULA IPv6 keyspace. The 16-byte MMDB key is constructed as:
  - bytes 0–9: fixed ULA prefix `fd:ac:db:00:00:00:00:00:00:00` (10 bytes, 80 bits)
  - bytes 10–15: the 6 bytes from `address_min` (padded with trailing zeros for blocks shorter than 48 bits)
  - CIDR mask width: `80 + prefixBits` where `prefixBits = 48 - log2(block_size)`
  - Examples: MA-L (24-bit prefix) → `/104`; MA-M (28-bit) → `/108`; MA-S (36-bit) → `/116`
  - For lookup: the full 6-byte MAC is placed at bytes 10–15 of the same prefix, then queried as `/128`
- **Custom mmdb**: built with `github.com/maxmind/mmdbwriter` and read with `github.com/oschwald/maxminddb-golang`. **Important**: in `mmdbwriter v1.0.0` the insertion method is `writer.Insert(network, record)` — not `InsertNetwork`.
- **Two update modes** controlled by `MAC_DB_URL`:
  - **CDN CSV mode** (default): `buildMACDBFromCSV` fetches a gzipped CSV from `https://cdn.letstool.net/mac/csv`, decompresses it on the fly, parses it with `encoding/csv`, and compiles a fresh `mac.mmdb`.
  - **Peer mode** (`MAC_DB_URL` set): `downloadFromPeer` downloads `mac.mmdb` from `/db/mac` of another `http2mac` instance.
- **CDN protocol**: `fetchCSVFromCDN` sends `If-Modified-Since` (read from `.last_modified_tor`) on every request. The switch on the CDN status code handles five cases:
  - **304 Not Modified** — costs no quota; treated as success (timestamp refreshed, build skipped). Returns the sentinel `errNotModified`.
  - **429 Too Many Requests** — returns `*errRateLimited` containing the `Retry-After` unix timestamp.
  - **410 Gone** — product is disabled on the CDN side; returns `*errProductGone` with the JSON body message.
  - **401 Unauthorized** — license level insufficient; returns `*errUnauthorized` with the human-readable `message` field extracted from the CDN JSON body via `extractJSONMessage`.
  - **200 OK** — `Last-Modified` is stored in `.last_modified_tor` for subsequent `If-Modified-Since` requests; CSV stream returned to caller.
- **Hot database swap**: the active `*maxminddb.Reader` is stored in a `sync/atomic.Value` via `swapDB()`.
- **MAC normalisation**: `normalizeMAC()` accepts five formats, strips separators, validates 6-byte length and hex encoding, and returns a `net.HardwareAddr`.
- **Unicast / Multicast detection**: the LSB of the first MAC octet determines type — `0` = Unicast, `1` = Multicast (includes broadcast FF:FF:FF:FF:FF:FF).
- **Registered flag**: `true` only when the MMDB record has `assignment` ∈ {MA-L, MA-M, MA-S, CID, IAB}. Locally administered addresses (LAA) and multicast addresses will typically not match any block and return `registered: false`.
- **`virtual` field**: stored and returned as a raw string from the CSV, not a boolean. `"False"` indicates no known virtualisation association; any other value names the hypervisor (e.g. `"VMware"`).

---

## Environment variables & CLI flags

Every configuration value can be set via an environment variable **or** a command-line flag. The flag always takes priority. Resolution order: **CLI flag → environment variable → hard-coded default**.

| Environment variable | CLI flag        | Default          | Description                                                |
|----------------------|-----------------|------------------|------------------------------------------------------------|
| `LISTEN_ADDR`        | `-listen-addr`  | `127.0.0.1:8080` | Listen address and port for the HTTP server.               |
| `MAC_DB_DIR`         | `-db-dir`       | `/data`          | Directory used to store and cache the `mac.mmdb` file.     |
| `MAC_DB_URL`         | `-db-url`       | *(none)*         | Base URL of a peer http2mac instance. When set, enables peer mode. |
| `LICENSE_KEY`        | `-license-key`  | *(none)*         | CDN license token. Sent as `Authorization: Basic <token>`. |
| `MAC_MAX_MACS`       | `-max-macs`     | `100`            | Maximum MACs accepted in a single batch request.           |

**Proxy variables** (no CLI flag — curl-compatible convention):

| Variable | Description |
|---|---|
| `HTTPS_PROXY` / `https_proxy` | Proxy URL for HTTPS traffic. |
| `HTTP_PROXY` / `http_proxy`   | Proxy URL for plain HTTP traffic. |
| `NO_PROXY` / `no_proxy`       | Comma-separated bypass list. |

---

## Data model

### mmdb record schema (`MACRecord`)

Each OUI block is stored in `mac.mmdb` with the following fields:

| mmdb key               | Go type  | Description                                           |
|------------------------|----------|-------------------------------------------------------|
| `oui`                  | string   | OUI prefix (e.g. `00:11:22`)                          |
| `organisation_name`    | string   | Registered organisation name                          |
| `organization_address` | string   | Organisation postal address                           |
| `country_code`         | string   | ISO 3166-1 alpha-2 country code                       |
| `address_min`          | string   | Lowest MAC address in the block                       |
| `address_max`          | string   | Highest MAC address in the block                      |
| `block_size`           | uint64   | Number of MAC addresses in the block                  |
| `assignment`           | string   | MA-L, MA-M, MA-S, CID, or IAB                         |
| `virtual`              | string   | `"False"` or hypervisor name                          |

### CSV source format

The CDN serves a gzipped CSV at `https://cdn.letstool.net/mac/csv` with exactly 9 columns:

```
oui,organisation_name,organization_address,country_code,address_min,address_max,block_size,assignment,virtual
```

- `oui` uses `:` as byte separator (e.g. `00:00:00`)
- `address_min` / `address_max` are full 6-byte MACs with `:` separator
- `virtual` is a string — `"False"` or a virtualisation technology name; **not** a boolean

### IPv6 MMDB keyspace

```
key layout (16 bytes):
  [0-9]  = fd:ac:db:00:00:00:00:00:00:00   (fixed ULA prefix)
  [10-15] = address_min bytes (zero-padded to 6 bytes)

CIDR mask = /80 + prefixBits
prefixBits = 48 - log2(block_size)

Examples:
  MA-L (block_size=16777216, 24-bit prefix)  → /104
  MA-M (block_size=1048576,  28-bit prefix)  → /108
  MA-S (block_size=4096,     36-bit prefix)  → /116

Lookup key = prefix(10) + MAC(6) queried as /128
```

---

## API contract

### Endpoint

```
POST /api/v1/mac
Content-Type: application/json
```

Exactly one of `mac` or `macs` must be provided per request.

### Accepted MAC formats

| Format | Example |
|--------|---------|
| Colon-separated | `00:11:22:33:44:55` |
| Hyphen-separated | `00-11-22-33-44-55` |
| Dot-separated (per-byte) | `00.11.22.33.44.55` |
| Cisco (4-digit groups) | `0011.2233.4455` |
| No separator | `001122334455` |

### Response status values

| Value      | Meaning                                                                     |
|------------|-----------------------------------------------------------------------------|
| `SUCCESS`  | At least one MAC found in the OUI database; `answers` contains DB data     |
| `NOTFOUND` | No MACs matched any OUI block; `answers` still contains validation/type    |
| `ERROR`    | Request malformed, database not yet initialised, or batch size exceeded    |

### Additional response fields per MAC answer

| Field        | Type    | Description                                                   |
|--------------|---------|---------------------------------------------------------------|
| `valid`      | bool    | Format validation passed                                      |
| `admin_type` | string  | `"UAA"`, `"LAA"`                                              |
| `type`       | string  | `"Unicast"`, `"Multicast"`, or `"Unknown"` (for invalid MACs)|
| `registered` | bool    | Found in MA-L / MA-M / MA-S / CID / IAB block                |

### Other endpoints

| Method | Path            | Description                                               |
|--------|-----------------|-----------------------------------------------------------|
| `GET`  | `/`             | Embedded interactive web UI                               |
| `GET`  | `/openapi.json` | OpenAPI 3.1 specification                                 |
| `GET`  | `/favicon.png`  | Application icon                                          |
| `GET`  | `/db/mac`       | Serves the current `mac.mmdb` for peer download            |

---

## Web UI

The UI is a self-contained single-file HTML/JS/CSS application embedded in the binary.

- **Themes**: dark and light, switchable via toggle. Dark theme uses a green accent (`#00e5a0`).
- **Languages**: 15 locales — Arabic (`ar`), Bengali (`bn`), German (`de`), English (`en`), Spanish (`es`), French (`fr`), Hindi (`hi`), Indonesian (`id`), Japanese (`ja`), Korean (`ko`), Portuguese (`pt-BR`), Russian (`ru`), Urdu (`ur`), Vietnamese (`vi`), Chinese (`zh-CN`).
- **RTL support**: Arabic and Urdu switch layout to right-to-left.
- **Input modes**: single MAC or multi-MAC (textarea). Multi accepts one per line, or comma/space-separated. All five MAC formats are accepted.
- **Results table**: MAC address (normalised) · Valid badge · Type badge · Registered badge · Organisation · Country · Assignment pill · Virtual chip.
- **Stats bar**: breakdown by registered, unregistered, unicast, multicast, invalid counts.
- **Copy-to-CSV**: exports all results in pipe-delimited format.

---

## Scheduler

The update scheduler fires `updateDB` every `updateInterval` (24 hours). CDN status codes alter the schedule:

- **304 Not Modified** — skips rebuild, refreshes the local timestamp. Costs no CDN quota.
- **429 Too Many Requests** — defers next attempt to the CDN-supplied `Retry-After` timestamp.
- **410 Gone** — retries after 24 h, 48 h, 72 h, 96 h, then stops permanently.
- **401 Unauthorized** — logs the server message and stops permanently.
- **Any other error** — logs and retries after the normal interval.

Marker files in `MAC_DB_DIR`:
- `.last_update_mac` — Unix timestamp of the last successful build/download.
- `.last_modified_mac` — `Last-Modified` HTTP header from the last CDN 200 response; sent as `If-Modified-Since`.

---

## Constraints & conventions

- Go version: **1.24+**
- No `cgo`. Keep `CGO_ENABLED=0`.
- No additional HTTP frameworks or routers.
- All logic stays in `cmd/http2mac/main.go`.
- Error responses always return a `MACResponse` JSON body — never plain-text.
- The server never logs request bodies; avoid logging queried MAC addresses.
- All code, identifiers, comments, and documentation must be in **English**.
- Every configuration environment variable must have a corresponding CLI flag.

---

## Build & run commands

```bash
# Initialise / tidy dependencies
bash scripts/000_init.sh

# Build native static binary -> ./out/http2mac
bash scripts/linux_build.sh

# Run
bash scripts/linux_run.sh

# Build Docker image -> letstool/http2mac:latest
bash scripts/docker_build.sh

# Run Docker container
bash scripts/docker_run.sh

# Smoke tests (server must be running)
bash scripts/999_test.sh
```

---

## AI-assisted development

This project was developed with the assistance of **Claude Sonnet 4.6** by Anthropic.
