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
```

## Status

Day-one scope: ingest the **metadata** for every RFC and every active
Internet-Draft into queryable YAML records. The MCP server, full-text
extraction, and structured-element extraction land in subsequent
phases.

## Using it

```bash
go run ./cmd/ietf-crawl --corpus .
```

This populates `corpus/rfcs/` with one YAML per RFC and `corpus/drafts/`
with one YAML per active draft. Re-running is safe: existing records
are updated in place, deltas are detected, and drafts whose state has
changed (active → expired / rfc / replaced) are moved out of the
active set on the next sync.

## License

Schema, taxonomy, and corpus records: **CC0 / public domain**.

The records mirror metadata from the IETF Trust's published documents
(RFCs and Internet-Drafts), which are themselves under the [IETF Trust
Legal Provisions](https://trustee.ietf.org/license-info/). The corpus
does not redistribute the source documents themselves — each record
points at the canonical URL on rfc-editor.org or datatracker.ietf.org.

LLM-extracted elements (when they land) are CC-BY-4.0 with attribution
to the source RFC or draft.
