# ietf-corpus

A structured, LLM-callable corpus of every IETF RFC and active Internet-Draft.

This is a research index, not a document host. Each entry is a YAML
record with the document's canonical metadata, IETF-native controlled
vocabularies (stream, status, area, working group), and the
updates/obsoletes/replaces graph. The whole thing is exposed through an
MCP server so an LLM can answer questions like "which RFCs define the
HTTP/2 connection lifecycle?" or "what's the current standards-track
state of every QUIC document?" without anyone re-reading the entire
RFC series.

This corpus is general-purpose. It's maintained by Lantern but has no
Lantern-specific tags or relevance scoring — the schema is purely IETF
metadata plus optional cross-cutting topic tags anyone can extend.

## Sources

- **RFCs** — `https://www.rfc-editor.org/rfc-index.xml` (the canonical
  RFC Editor index). One YAML per RFC in `corpus/rfcs/`.
- **Internet-Drafts** — `datatracker.ietf.org` JSON API
  (`/api/v1/doc/document/?type=draft&states__slug=active`). One YAML per
  active draft in `corpus/drafts/`. Drafts expire after 6 months; the
  crawler resyncs to keep the directory current and prunes records
  whose underlying drafts have expired or been replaced.

## How it's organized

```
schema/              JSON Schemas + the YAML taxonomy. The durable artifact.
  rfc.schema.json
  draft.schema.json
  element.schema.json    (placeholder; LLM-extracted elements come later)
  taxonomy.yaml          IETF streams, areas, topics, element kinds.
corpus/
  rfcs/                  One YAML per RFC.
  drafts/                One YAML per active Internet-Draft.
  elements/              (Optional) LLM-extracted elements, one YAML per element.
cmd/ietf-crawl/      Fetches both sources and writes the YAML records.
cmd/ietf-mcp/        Local stdio MCP server reading the YAMLs.
```

## Status

Day-one scope, shipped:

- `cmd/ietf-crawl` populates `corpus/rfcs/` (9,768 RFCs) and
  `corpus/drafts/` (~2.5k active Internet-Drafts).
- `cmd/ietf-mcp` exposes the corpus over MCP with five tools.
- LLM-extracted structured elements (the `corpus/elements/` slot) are
  next.

## Populating the corpus

```bash
go run ./cmd/ietf-crawl --corpus .
```

This writes one YAML per RFC and per active draft. Re-running is safe:
existing records are updated in place, and drafts whose state has
changed (active → expired / rfc / replaced) are pruned on the next
sync.

## Using the MCP server

The MCP server is designed to be code-mode-friendly (see Cloudflare's
[Code Mode](https://blog.cloudflare.com/code-mode/) post and
Anthropic's [Code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp)),
which means: five small composable primitives, structured JSON outputs,
rich JSON Schemas, real cursor-based pagination on search. Works with
traditional MCP clients today and benefits automatically when the
agent harness has code-mode support.

```bash
go install github.com/getlantern/ietf-corpus/cmd/ietf-mcp@latest
git clone https://github.com/getlantern/ietf-corpus ~/code/ietf-corpus

claude mcp add -s user ietf-corpus \
  $(go env GOPATH)/bin/ietf-mcp -- --corpus $HOME/code/ietf-corpus
```

### Tools

| Tool | Purpose |
| --- | --- |
| `search_documents` | Free-text + tag-filter search. Cursor-paginated. Returns `Document` records with a `type` discriminator (`rfc` or `draft`). |
| `get_document` | Fetch one full record by id. |
| `get_documents` | Batch-fetch by id list. Returns `{documents, not_found}`. Use this instead of a loop of `get_document` calls when expanding relationship graphs. |
| `find_related` | Expand the IETF relationship edges (obsoletes, obsoleted_by, updates, updated_by, also, replaces, replaced_by, predecessor_draft, successor_rfc). Each kind returns the full target documents — no second fetch needed. |
| `list_taxonomy` | Controlled vocabularies (streams, areas, topics, element_kinds). |

Document ids are `rfc-<number>` for RFCs and the base name (no
revision suffix) for drafts, e.g. `draft-ietf-tls-esni`.

## Browsable site

A static site rendered from the same YAMLs is served from Cloudflare
Pages. To build locally:

```bash
make site            # renders to ./dist/
python3 -m http.server -d dist 8080
```

The site uses `cmd/ietf-site/`, which reuses the same YAML loading
the MCP server uses. Go html/template, no JS framework, no
`node_modules`. About 4 seconds to render the full 12k-document
corpus into 50 MB of static HTML.

### Cloudflare Pages setup (one-time)

The deploy uses CF Pages' native git integration — no API tokens or
GitHub Actions secrets required. CF watches the repo and rebuilds on
every push to `main`.

To wire it up:

1. In the Cloudflare dashboard, **Workers & Pages** → **Create
   application** → **Pages** → **Connect to Git** → select
   `getlantern/ietf-corpus`.
2. Build settings:
   - **Framework preset**: None
   - **Build command**: `go run ./cmd/ietf-site --corpus . --out dist`
   - **Build output directory**: `dist`
   - **Root directory**: `/`
3. Save and deploy.

The build runs in CF's gVisor sandbox with Go 1.24.3 preinstalled
(v3 build image, no `GO_VERSION` env var needed). Every PR also gets
an automatic preview deployment at `<branch>.<project>.pages.dev`.

## License

Schema, taxonomy, and corpus records: **CC0 / public domain**.

The records mirror metadata from the IETF Trust's published documents
(RFCs and Internet-Drafts), which are themselves under the [IETF Trust
Legal Provisions](https://trustee.ietf.org/license-info/). The corpus
does not redistribute the source documents themselves — each record
points at the canonical URL on rfc-editor.org or datatracker.ietf.org.

LLM-extracted elements (when they land) are CC-BY-4.0 with attribution
to the source RFC or draft.
