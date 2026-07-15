# scrapr

An **agentic web scraper** as a holistic service. scrapr crawls websites for data, files,
media, images, texts and documents — but instead of hand-written selectors, an **LLM drives the
crawl**: for every page the model decides which links to follow, what to download and what to
extract. scrapr is "the hands" (fetch / parse / download / store); [`aigentic`](../aigentic)'s
`choose` router (ollama → Claude escalation) is "the brain".

scrapr is **domain-agnostic** — a generic scraper service usable on its own — and is also
**consumed by [studiq](../studiq)**: each scraper created in studiq's Fuse tab is a job the one
`scraprd` daemon runs.

```
Browser (e.g. studiq Fuse tab)
   │  /api/services/scrapr/*   (same-origin, h_access JWT + CSRF)
   ▼
Caddy ──► scraprd  (127.0.0.1:8782)
            ├─ holistic shell: auth (h_access), rights (hp_scrapr_*), CSRF, health
            ├─ SQLite index (/var/lib/scrapr/index.db): scrapers · runs · document metadata
            ├─ prizm registry:
            │     ├─ aigentic leaves + choose   (the LLM "brain")
            │     └─ scrape processor            (the agentic ReAct crawl loop)
            └─ graveyard = lakearch (cgo)        → artifact bytes, BLAKE3 content-id
```

## How the crawl works

Built as a **derived prizm** (like aigentic). The `scrape` processor is a coordinator/worker
split over one registry:

- **coordinator** (`In.Job`) — BFS over the frontier; owns per-run state (visited set, budgets,
  allowlist); fans pages out with `subprizm.SpawnAll`.
- **worker** (`In.Page`) — *fetch → parse → decide → act*. It enumerates the page's candidate
  links and assets, asks the `choose` router for a strict JSON action, then executes it: keep the
  page (store its markdown), download selected assets, return accepted follow-links.

The LLM references candidates **by index**, and scrapr resolves the URL from its own list — so
the model can never invent an off-domain URL. Hard per-run **budgets** (pages, depth, fan-out,
bytes, deadline) plus an allowlist, visited-set dedupe, robots.txt and per-host rate-limiting
guarantee termination and politeness regardless of what the model returns. prizm recursion stays
flat: coordinator(0) → worker(1) → choose(2) → leaf(3).

## Storage

- **Artifact bytes → lakearch** (content-addressed, append-only; via aigentic's cgo binding,
  built with `-tags lakearch`). The returned BLAKE3 content-id is the durable `Ref`.
- **Metadata → SQLite** (`modernc.org/sqlite`, WAL): scraper configs, run history and document
  metadata (title, kategorie, provenance, `lakearchRef`), every row scoped to its owner.
- **Caller-designated store (M2):** a scraper carries a `storeRef`; studiq will later hand scrapr
  a reference to *its own* lakearch so scraped `Quelle`s land there (a shared store is single-writer,
  so it goes via `lakearchd` gRPC — see Milestones).

## LLM engines

The decision + classification calls go through aigentic's `choose` router. Configure **at least
one** engine (env, set by the systemd unit):

| var | meaning |
|---|---|
| `OLLAMA_HOST` / `SCRAPR_OLLAMA_MODEL` | local ollama (free, default classifier) |
| `ANTHROPIC_API_KEY` / `SCRAPR_CLAUDE_MODEL` | Anthropic API (metered) |
| `SCRAPR_ENGINE` | pin the leaf (`ollama`\|`claude-cli`\|`claude-api`); empty = estimate |

With no engine reachable, `trigger` returns 503.

## Quickstart

```bash
# Dev (pure-Go, in-memory store — no Rust/cgo needed):
cd backend
SCRAPR_GRAVEYARD=memory go build ./... && go test ./...

# Production build + deploy (embeds lakearch):
sudo ./service setup        # cargo-builds lakearch FFI, builds scraprd -tags lakearch,
                            # wires systemd + Caddy (127.0.0.1:8782) + rights, links no UI (headless M1)
```

`GO_TAGS=""` builds pure-Go (memory store) for a host without Rust. Lifecycle: `service
build|start|stop|restart|status|update|uninstall`.

## HTTP API (`/api/services/scrapr/*`)

studiq-shaped JSON (its `Scraper` / `Document` / `ScraperRun` types). Reads gate on
`hp_scrapr_use`, writes/trigger on `hp_scrapr_run` + CSRF.

| method | path | studiq DataSource |
|---|---|---|
| GET | `health` | (public) |
| GET | `scrapers` | `scrapers()` |
| POST | `scrapers` | `addScraper()` |
| POST\|PATCH | `scrapers/{id}` | `updateScraper()` |
| POST | `scrapers/{id}/trigger` | `triggerScraper()` → runs the crawl synchronously |
| GET | `documents` | `documents()` |
| POST | `documents` | `addManualDocument()` |
| GET | `documents/{id}/content` | fetch bytes from the graveyard |
| — | `internal/*` | daemon→daemon (shared secret); studiqd path, stubbed until M2 |

## studiq integration

studiq's `src/data/httpSource.ts` implements the six FUSE methods against `/api/services/scrapr/*`;
`src/data/index.ts` routes per-method (FUSE → scrapr when reachable + signed-in, else mock). studiq
must be served **same-origin behind the holistic Caddy** (or the vite dev proxy → your Caddy, via
`STUDIQ_API_TARGET`) so the `h_access` cookie flows.

## Milestones

- **M1 (this):** headless scraprd + agentic loop over `choose` + SQLite/lakearch + studiq Fuse
  wiring, tested on Wikipedia (public, no auth).
- **M2:** caller-designated lakearch store via `lakearchd` gRPC (generate the Go client) +
  studiqarch `Quelle` mapping + scheduling (`scheduleKind`) + async runs.
- **M3 (Moodle):** login/credentials + a `chromedp` headless-browser `Fetcher` for JS/auth pages
  + course→section→resource navigation.

## Layout

```
service                    single-file CLI: init / setup / build / lifecycle
permissions/scrapr.json    rights manifest (hp_scrapr_use, hp_scrapr_run)
backend/                   Go daemon (scraprd), module `scrapr`
  cmd/scraprd/             entry point — 127.0.0.1:8782
  internal/auth/           shared-JWT + live group/admin + CSRF (from template)
  internal/rights/         hp_scrapr_* group constants
  internal/api/            HTTP routes + studiq-shaped JSON
  internal/store/          SQLite metadata index (scrapers/runs/documents)
  internal/grave/          graveyard selection (memory | lakearch)
  scrape/                  the agentic crawl: coordinator/worker loop, fetch, extract,
                           robots + rate-limit, LLM decision, guardrails
```

## License

MIT — see [LICENSE](LICENSE).
