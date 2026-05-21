// ietf-mcp is the MCP server for the IETF corpus.
//
// It exposes the corpus over stdio as an MCP server. v0 reads YAML
// files from disk at startup and serves from in-memory indexes; with
// ~12k records that's a comfortable few-MB working set. A SQLite-FTS
// or D1-backed implementation becomes worthwhile only if startup
// latency or per-query scan time start to matter.
//
// Tool design notes (code-mode-friendly):
//
//   - Small composable primitives over fat mega-tools, so an LLM
//     writing code can chain them naturally.
//   - Rich JSON Schemas on every input/output — they become the
//     TypeScript API surface that code-mode generates.
//   - Structured outputs (JSON objects, not formatted prose) so
//     code can .map() / .filter() over them.
//   - Cursor-based pagination on search_documents — real pagination,
//     not truncation, because code will actually use it.
//   - get_documents accepts a list of ids; with code mode it's a
//     batch-fetch idiom that beats a tool-call loop.
//
// Usage:
//
//	ietf-mcp --corpus /path/to/ietf-corpus
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	serverName    = "ietf-corpus"
	serverVersion = "0.1.0"
	protocolRev   = "2024-11-05"
)

// ─── In-memory document shape ────────────────────────────────────────────
//
// The MCP layer normalizes RFCs and drafts into a single Document shape
// with a `type` discriminator. Common fields live at the top level; the
// fields that exist only on one type are populated only for that type
// and omitted (omitempty) otherwise. This lets code-mode see a clean
// discriminated union in TypeScript.

type Document struct {
	ID       string   `json:"id" yaml:"id"`
	Type     string   `json:"type" yaml:"-"` // "rfc" or "draft"
	Title    string   `json:"title" yaml:"title"`
	Date     string   `json:"date,omitempty" yaml:"date,omitempty"`
	Year     int      `json:"year,omitempty" yaml:"-"`
	Authors  []Author `json:"authors,omitempty" yaml:"authors,omitempty"`
	Abstract string   `json:"abstract,omitempty" yaml:"abstract,omitempty"`
	URL      string   `json:"url,omitempty" yaml:"url,omitempty"`
	Stream   string   `json:"stream,omitempty" yaml:"stream,omitempty"`
	Area     string   `json:"area,omitempty" yaml:"area,omitempty"`
	WG       string   `json:"wg,omitempty" yaml:"wg,omitempty"`
	Keywords []string `json:"keywords,omitempty" yaml:"keywords,omitempty"`
	Topics   []string `json:"topics,omitempty" yaml:"topics,omitempty"`

	// RFC-only fields. Populated when Type == "rfc".
	RFCNumber         int      `json:"rfc_number,omitempty" yaml:"rfc_number,omitempty"`
	CurrentStatus     string   `json:"current_status,omitempty" yaml:"current_status,omitempty"`
	PublicationStatus string   `json:"publication_status,omitempty" yaml:"publication_status,omitempty"`
	PageCount         int      `json:"page_count,omitempty" yaml:"page_count,omitempty"`
	Formats           []string `json:"formats,omitempty" yaml:"formats,omitempty"`
	Draft             string   `json:"draft,omitempty" yaml:"draft,omitempty"`
	Obsoletes         []string `json:"obsoletes,omitempty" yaml:"obsoletes,omitempty"`
	ObsoletedBy       []string `json:"obsoleted_by,omitempty" yaml:"obsoleted_by,omitempty"`
	Updates           []string `json:"updates,omitempty" yaml:"updates,omitempty"`
	UpdatedBy         []string `json:"updated_by,omitempty" yaml:"updated_by,omitempty"`
	Also              []string `json:"also,omitempty" yaml:"also,omitempty"`
	DOI               string   `json:"doi,omitempty" yaml:"doi,omitempty"`
	ErrataURL         string   `json:"errata_url,omitempty" yaml:"errata_url,omitempty"`

	// Draft-only fields. Populated when Type == "draft".
	Name           string   `json:"name,omitempty" yaml:"name,omitempty"`
	Rev            string   `json:"rev,omitempty" yaml:"rev,omitempty"`
	StreamState    string   `json:"stream_state,omitempty" yaml:"stream_state,omitempty"`
	Group          string   `json:"group,omitempty" yaml:"group,omitempty"`
	IntendedStatus string   `json:"intended_status,omitempty" yaml:"intended_status,omitempty"`
	FirstSubmitted string   `json:"first_submitted,omitempty" yaml:"first_submitted,omitempty"`
	LastUpdated    string   `json:"last_updated,omitempty" yaml:"last_updated,omitempty"`
	Expires        string   `json:"expires,omitempty" yaml:"expires,omitempty"`
	Replaces   []string `json:"replaces,omitempty" yaml:"replaces,omitempty"`
	ReplacedBy []string `json:"replaced_by,omitempty" yaml:"replaced_by,omitempty"`
	Pages      int      `json:"pages,omitempty" yaml:"pages,omitempty"`
	Words      int      `json:"words,omitempty" yaml:"words,omitempty"`
	// NB: RFCNumber serves both types. For RFCs it's the RFC's own
	// number; for drafts it's the RFC number the draft was published as
	// (populated only after the draft transitions to state=rfc).

	// Elements are LLM-extracted normative requirements / protocol
	// elements / considerations / design rationale for this document.
	// Populated only for documents that have been through ietf-elements;
	// absent for the long tail.
	Elements []*Element `json:"elements,omitempty" yaml:"-"`

	// haystack is a lowercased blob of the document's searchable text,
	// built once at load time so substring search doesn't reallocate.
	haystack string `json:"-" yaml:"-"`
}

// Element mirrors schema/element.schema.json and corpus/elements/*.yaml.
type Element struct {
	ID           string   `json:"id" yaml:"-"` // derived from filename
	Document     string   `json:"document" yaml:"document"`
	Kind         string   `json:"kind" yaml:"kind"`
	Summary      string   `json:"summary" yaml:"summary"`
	Section      string   `json:"section,omitempty" yaml:"section,omitempty"`
	RFC2119Level string   `json:"rfc2119_level,omitempty" yaml:"rfc2119_level,omitempty"`
	Topics       []string `json:"topics,omitempty" yaml:"topics,omitempty"`
	ExtractedBy  string   `json:"extracted_by,omitempty" yaml:"extracted_by,omitempty"`
	ExtractedAt  string   `json:"extracted_at,omitempty" yaml:"extracted_at,omitempty"`

	haystack string `json:"-" yaml:"-"`
}

type Author struct {
	Name        string `json:"name" yaml:"name"`
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Email       string `json:"email,omitempty" yaml:"email,omitempty"`
	Affiliation string `json:"affiliation,omitempty" yaml:"affiliation,omitempty"`
}

// ─── Store ───────────────────────────────────────────────────────────────

type store struct {
	mu        sync.RWMutex
	docs      []*Document
	byID      map[string]*Document
	elements  []*Element
	taxonomy  map[string]any
	corpusDir string
}

func loadStore(corpusDir string) (*store, error) {
	s := &store{byID: map[string]*Document{}, corpusDir: corpusDir}

	start := time.Now()
	if err := s.loadDir(filepath.Join(corpusDir, "corpus", "rfcs"), "rfc"); err != nil {
		return nil, err
	}
	if err := s.loadDir(filepath.Join(corpusDir, "corpus", "drafts"), "draft"); err != nil {
		return nil, err
	}

	// Stable sort by id for deterministic search results.
	sort.Slice(s.docs, func(i, j int) bool { return s.docs[i].ID < s.docs[j].ID })

	// Elements: load + attach to their owning document. Missing
	// elements dir is fine (most documents won't have any).
	if err := s.loadElements(filepath.Join(corpusDir, "corpus", "elements")); err != nil {
		return nil, err
	}

	// Taxonomy.
	taxPath := filepath.Join(corpusDir, "schema", "taxonomy.yaml")
	if raw, err := os.ReadFile(taxPath); err == nil {
		if err := yaml.Unmarshal(raw, &s.taxonomy); err != nil {
			return nil, fmt.Errorf("parse taxonomy: %w", err)
		}
	}

	log.Printf("ietf-mcp: loaded %d documents from %s in %s", len(s.docs), corpusDir, time.Since(start).Round(time.Millisecond))
	return s, nil
}

func (s *store) loadDir(dir, docType string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		var d Document
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		d.Type = docType
		d.Year = deriveYear(&d)
		d.haystack = buildHaystack(&d)
		if _, dup := s.byID[d.ID]; dup {
			return fmt.Errorf("%s: duplicate id %s", e.Name(), d.ID)
		}
		s.byID[d.ID] = &d
		s.docs = append(s.docs, &d)
	}
	return nil
}

func (s *store) loadElements(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read elements dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		var el Element
		if err := yaml.Unmarshal(raw, &el); err != nil {
			return fmt.Errorf("parse element %s: %w", e.Name(), err)
		}
		el.ID = strings.TrimSuffix(e.Name(), ".yaml")
		el.haystack = strings.ToLower(el.Summary + " " + el.Section + " " + el.RFC2119Level + " " + strings.Join(el.Topics, " "))
		s.elements = append(s.elements, &el)
		if d, ok := s.byID[el.Document]; ok {
			d.Elements = append(d.Elements, &el)
		}
	}
	return nil
}

// deriveYear pulls a 4-digit year from whichever date field is present.
// RFCs use `date` (YYYY-MM or YYYY-MM-DD); drafts use `last_updated`.
func deriveYear(d *Document) int {
	candidates := []string{d.Date, d.LastUpdated, d.FirstSubmitted}
	for _, c := range candidates {
		if len(c) >= 4 {
			var y int
			if _, err := fmt.Sscanf(c[:4], "%d", &y); err == nil && y > 1960 && y < 2100 {
				return y
			}
		}
	}
	return 0
}

func buildHaystack(d *Document) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(d.ID))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(d.Title))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(d.Abstract))
	for _, a := range d.Authors {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(a.Name))
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(a.Affiliation))
	}
	for _, k := range d.Keywords {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(k))
	}
	for _, t := range d.Topics {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(t))
	}
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(d.WG + " " + d.Group + " " + d.Area + " " + d.Stream))
	return b.String()
}

// ─── Search ──────────────────────────────────────────────────────────────

type searchArgs struct {
	Query     string   `json:"query"`
	Type      string   `json:"type"` // "rfc" | "draft" | "" (all)
	Stream    string   `json:"stream"`
	Status    string   `json:"status"` // matches current_status or stream_state
	Area      string   `json:"area"`
	WG        string   `json:"wg"`
	Topic     string   `json:"topic"`
	YearMin   int      `json:"year_min"`
	YearMax   int      `json:"year_max"`
	IDs       []string `json:"ids"` // pre-filter to a specific id set (intersect with other filters)
	Limit     int      `json:"limit"`
	Cursor    string   `json:"cursor"` // offset as string; matches next_cursor in response
}

type searchResponse struct {
	Results    []*Document `json:"results"`
	Total      int         `json:"total"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (s *store) search(a searchArgs) searchResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 200 {
		a.Limit = 200
	}
	offset := 0
	if a.Cursor != "" {
		fmt.Sscanf(a.Cursor, "%d", &offset)
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	idSet := map[string]bool{}
	for _, id := range a.IDs {
		idSet[id] = true
	}

	var matched []*Document
	for _, d := range s.docs {
		if len(idSet) > 0 && !idSet[d.ID] {
			continue
		}
		if a.Type != "" && d.Type != a.Type {
			continue
		}
		if a.Stream != "" && !strings.EqualFold(d.Stream, a.Stream) {
			continue
		}
		if a.Status != "" {
			if !strings.EqualFold(d.CurrentStatus, a.Status) && !strings.EqualFold(d.StreamState, a.Status) && !strings.EqualFold(d.IntendedStatus, a.Status) {
				continue
			}
		}
		if a.Area != "" && !strings.EqualFold(d.Area, a.Area) {
			continue
		}
		if a.WG != "" && !strings.EqualFold(d.WG, a.WG) && !strings.EqualFold(d.Group, a.WG) {
			continue
		}
		if a.Topic != "" && !slices.Contains(d.Topics, strings.ToLower(a.Topic)) {
			continue
		}
		if a.YearMin > 0 && d.Year > 0 && d.Year < a.YearMin {
			continue
		}
		if a.YearMax > 0 && d.Year > 0 && d.Year > a.YearMax {
			continue
		}
		if q != "" && !strings.Contains(d.haystack, q) {
			continue
		}
		matched = append(matched, d)
	}

	// Sort: newer first, then by id for stability.
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Year != matched[j].Year {
			return matched[i].Year > matched[j].Year
		}
		return matched[i].ID < matched[j].ID
	})

	total := len(matched)
	end := offset + a.Limit
	if end > total {
		end = total
	}
	if offset > total {
		offset = total
	}
	page := matched[offset:end]
	resp := searchResponse{Results: page, Total: total}
	if end < total {
		resp.NextCursor = fmt.Sprintf("%d", end)
	}
	return resp
}

// ─── search_elements ─────────────────────────────────────────────────────

type elementSearchResponse struct {
	Results    []*Element `json:"results"`
	Total      int        `json:"total"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func (s *store) searchElements(query, kind, level, doc, topic string, limit int, cursor string) elementSearchResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
	}
	q := strings.ToLower(strings.TrimSpace(query))
	topic = strings.ToLower(topic)
	level = strings.ToUpper(level)

	var matched []*Element
	for _, e := range s.elements {
		if kind != "" && !strings.EqualFold(e.Kind, kind) {
			continue
		}
		if level != "" && !strings.EqualFold(e.RFC2119Level, level) {
			continue
		}
		if doc != "" && e.Document != doc {
			continue
		}
		if topic != "" && !slices.Contains(e.Topics, topic) {
			continue
		}
		if q != "" && !strings.Contains(e.haystack, q) {
			continue
		}
		matched = append(matched, e)
	}
	total := len(matched)
	end := offset + limit
	if end > total {
		end = total
	}
	if offset > total {
		offset = total
	}
	resp := elementSearchResponse{Results: matched[offset:end], Total: total}
	if end < total {
		resp.NextCursor = fmt.Sprintf("%d", end)
	}
	return resp
}

// ─── find_related ────────────────────────────────────────────────────────

type relatedResponse struct {
	ID        string                 `json:"id"`
	Relations map[string][]*Document `json:"relations"`
}

func (s *store) findRelated(id string, kinds []string) (relatedResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.byID[id]
	if !ok {
		return relatedResponse{}, fmt.Errorf("document not found: %s", id)
	}
	// Default to all known kinds.
	if len(kinds) == 0 {
		kinds = []string{
			"obsoletes", "obsoleted_by", "updates", "updated_by", "also",
			"replaces", "replaced_by",
			"predecessor_draft", "successor_rfc",
		}
	}
	out := relatedResponse{ID: id, Relations: map[string][]*Document{}}
	expand := func(kind string, ids []string) {
		var docs []*Document
		for _, rid := range ids {
			if r, ok := s.byID[rid]; ok {
				docs = append(docs, r)
			}
		}
		out.Relations[kind] = docs
	}
	for _, k := range kinds {
		switch k {
		case "obsoletes":
			expand("obsoletes", d.Obsoletes)
		case "obsoleted_by":
			expand("obsoleted_by", d.ObsoletedBy)
		case "updates":
			expand("updates", d.Updates)
		case "updated_by":
			expand("updated_by", d.UpdatedBy)
		case "also":
			// BCP/STD/FYI aliases aren't loadable documents themselves —
			// return the raw id strings as a separate field so callers
			// can see the alias relationship without confusion.
			if len(d.Also) > 0 {
				// Wrap each alias as a stub Document so the shape stays uniform.
				var docs []*Document
				for _, a := range d.Also {
					docs = append(docs, &Document{ID: a, Type: aliasType(a), Title: a})
				}
				out.Relations["also"] = docs
			} else {
				out.Relations["also"] = nil
			}
		case "replaces":
			expand("replaces", d.Replaces)
		case "replaced_by":
			expand("replaced_by", d.ReplacedBy)
		case "predecessor_draft":
			// An RFC's `draft:` field points at the I-D it was published from.
			if d.Type == "rfc" && d.Draft != "" {
				// Strip any -NN revision suffix to match our base-name draft ids.
				name := stripDraftRev(d.Draft)
				if r, ok := s.byID[name]; ok {
					out.Relations["predecessor_draft"] = []*Document{r}
				} else {
					out.Relations["predecessor_draft"] = []*Document{{ID: d.Draft, Type: "draft", Title: d.Draft}}
				}
			} else {
				out.Relations["predecessor_draft"] = nil
			}
		case "successor_rfc":
			// A draft that was published as an RFC. For drafts, RFCNumber
			// holds the publication target (see Document.RFCNumber comment).
			if d.Type == "draft" && d.RFCNumber > 0 {
				rid := fmt.Sprintf("rfc-%d", d.RFCNumber)
				if r, ok := s.byID[rid]; ok {
					out.Relations["successor_rfc"] = []*Document{r}
				} else {
					out.Relations["successor_rfc"] = []*Document{{ID: rid, Type: "rfc"}}
				}
			} else {
				out.Relations["successor_rfc"] = nil
			}
		}
	}
	return out, nil
}

func aliasType(id string) string {
	switch {
	case strings.HasPrefix(id, "rfc-"):
		return "rfc"
	case strings.HasPrefix(id, "bcp-"):
		return "bcp"
	case strings.HasPrefix(id, "std-"):
		return "std"
	case strings.HasPrefix(id, "fyi-"):
		return "fyi"
	}
	return ""
}

// stripDraftRev returns the base name of a draft id, dropping a
// trailing "-NN" revision suffix. "draft-ietf-quic-transport-34"
// -> "draft-ietf-quic-transport".
func stripDraftRev(name string) string {
	for i := len(name) - 1; i > 0; i-- {
		c := name[i]
		if c == '-' {
			rest := name[i+1:]
			allDigit := len(rest) > 0
			for _, r := range rest {
				if r < '0' || r > '9' {
					allDigit = false
					break
				}
			}
			if allDigit {
				return name[:i]
			}
			break
		}
		if c < '0' || c > '9' {
			break
		}
	}
	return name
}

// ─── MCP wire types ──────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ─── Tool registry ───────────────────────────────────────────────────────
//
// Every input/output schema is intentionally specific. Code-mode
// generates TypeScript interfaces from these, so vague schemas
// produce vague TS — the LLM-written code degrades correspondingly.

var tools = []tool{
	{
		Name: "search_documents",
		Description: "Search the IETF corpus by free-text query and structured filters. Returns ranked Document records with type discriminator ('rfc' or 'draft'). Cursor-paginated — pass back next_cursor for the next page. With ~12k records and a typical filter, expect dozens to a few hundred matches; raise limit (max 200) if you need a bigger page.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"query":    map[string]any{"type": "string", "description": "Free-text substring search over id, title, abstract, authors, keywords, topics, working group, area, and stream. Case-insensitive."},
				"type":     map[string]any{"type": "string", "enum": []string{"rfc", "draft"}, "description": "Limit to a single document type. Omit to search both."},
				"stream":   map[string]any{"type": "string", "description": "Filter by publication stream: IETF, IAB, IRTF, Independent, Editorial, Legacy."},
				"status":   map[string]any{"type": "string", "description": "Filter by status. For RFCs matches current_status (PROPOSED STANDARD, INTERNET STANDARD, BEST CURRENT PRACTICE, INFORMATIONAL, EXPERIMENTAL, HISTORIC, UNKNOWN). For drafts matches stream_state (active, expired, ...) or intended_status."},
				"area":     map[string]any{"type": "string", "description": "IETF area acronym (sec, tsv, art, int, rtg, ops, gen, irtf)."},
				"wg":       map[string]any{"type": "string", "description": "Working group acronym (e.g. quic, tls, dnsop). Matches RFC.wg or draft.group."},
				"topic":    map[string]any{"type": "string", "description": "Cross-cutting topic tag from schema/taxonomy.yaml#topics (tls, http, quic, dns, ipsec, oauth, censorship, ...). Currently sparse; not every document has topics assigned."},
				"year_min": map[string]any{"type": "integer", "description": "Earliest publication year (inclusive)."},
				"year_max": map[string]any{"type": "integer", "description": "Latest publication year (inclusive)."},
				"ids":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Restrict the candidate set to these document ids (intersected with other filters). Useful when you've already got a candidate id list from get_documents or find_related and want to re-filter."},
				"limit":    map[string]any{"type": "integer", "default": 20, "maximum": 200, "description": "Maximum number of results to return in this page."},
				"cursor":   map[string]any{"type": "string", "description": "Pagination cursor returned by the previous call as next_cursor. Pass back to fetch the next page."},
			},
		},
	},
	{
		Name:        "get_document",
		Description: "Fetch a single Document by id. Returns the full record (all type-specific fields included). Ids look like 'rfc-9000' or 'draft-ietf-tls-esni'. Returns an error if no document with that id exists.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"id"},
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Document id. RFC ids: 'rfc-<number>'. Draft ids: the draft's base name without revision suffix, e.g. 'draft-ietf-tls-esni'."},
			},
		},
	},
	{
		Name: "get_documents",
		Description: "Batch-fetch multiple Documents by id. Returns a map of id → Document plus a list of ids that weren't found. Prefer this over a loop of get_document calls when you have a list of ids in hand — e.g. when expanding the obsoletes/updates graph from a starting document.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"ids"},
			"properties": map[string]any{
				"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of document ids to fetch."},
			},
		},
	},
	{
		Name: "find_related",
		Description: "Return the documents related to a given document by IETF relationship edges: obsoletes, obsoleted_by, updates, updated_by, also (BCP/STD/FYI aliases), replaces, replaced_by, predecessor_draft (for an RFC, the I-D it was published from), successor_rfc (for a draft, the RFC it became). Each kind returns an array of full Document records (so you can iterate without a second fetch).",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"id"},
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Document id to expand relations for."},
				"kinds": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string", "enum": []string{"obsoletes", "obsoleted_by", "updates", "updated_by", "also", "replaces", "replaced_by", "predecessor_draft", "successor_rfc"}},
					"description": "Which relationship kinds to expand. Omit to expand all known kinds. Use a subset when you only care about, e.g., the obsolescence chain.",
				},
			},
		},
	},
	{
		Name: "search_elements",
		Description: "Search the LLM-extracted structured elements (normative requirements, protocol elements, wire-format definitions, security/privacy considerations, design rationale, etc.) across the corpus. Useful for queries like 'find every MUST about TLS session tickets' or 'list all wire-format definitions in QUIC'. Elements are sparse — only documents that have been through ietf-elements have them; check coverage by counting per-doc results.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"query":         map[string]any{"type": "string", "description": "Free-text substring search over the element's summary, section, RFC 2119 level, and topics. Case-insensitive."},
				"kind":          map[string]any{"type": "string", "enum": []string{"normative-requirement", "protocol-element", "wire-format", "state-machine", "registry", "security-consideration", "privacy-consideration", "interoperability-note", "design-rationale", "errata"}, "description": "Restrict to one element kind."},
				"rfc2119_level": map[string]any{"type": "string", "enum": []string{"MUST", "MUST NOT", "SHOULD", "SHOULD NOT", "MAY", "REQUIRED", "RECOMMENDED", "OPTIONAL"}, "description": "For normative-requirement elements: restrict to a specific RFC 2119 keyword."},
				"document":      map[string]any{"type": "string", "description": "Restrict to elements from this document id (e.g. 'rfc-8446')."},
				"topic":         map[string]any{"type": "string", "description": "Restrict to elements tagged with this topic id."},
				"limit":         map[string]any{"type": "integer", "default": 30, "maximum": 200},
				"cursor":        map[string]any{"type": "string", "description": "Pagination cursor returned by the previous call."},
			},
		},
	},
	{
		Name: "list_taxonomy",
		Description: "Return the controlled-vocabulary taxonomy: streams, areas, topics, element_kinds. Call this once at the start of a session to discover the canonical id strings to filter by in search_documents.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"category": map[string]any{
					"type":        "string",
					"enum":        []string{"streams", "areas", "topics", "element_kinds"},
					"description": "Optional: return only one section of the taxonomy. Omit to return everything.",
				},
			},
		},
	},
}

// ─── Dispatch ────────────────────────────────────────────────────────────

func (s *store) handle(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolRev,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: err.Error()}
			return resp
		}
		out, err := s.callTool(p.Name, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": out}},
		}
	case "notifications/initialized":
		return rpcResponse{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func (s *store) callTool(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "search_documents":
		var a searchArgs
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
		}
		return jsonString(s.search(a))
	case "get_document":
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		s.mu.RLock()
		d, ok := s.byID[a.ID]
		s.mu.RUnlock()
		if !ok {
			return "", fmt.Errorf("document not found: %s", a.ID)
		}
		return jsonString(d)
	case "get_documents":
		var a struct {
			IDs []string `json:"ids"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		s.mu.RLock()
		found := map[string]*Document{}
		var missing []string
		for _, id := range a.IDs {
			if d, ok := s.byID[id]; ok {
				found[id] = d
			} else {
				missing = append(missing, id)
			}
		}
		s.mu.RUnlock()
		return jsonString(map[string]any{"documents": found, "not_found": missing})
	case "find_related":
		var a struct {
			ID    string   `json:"id"`
			Kinds []string `json:"kinds"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		out, err := s.findRelated(a.ID, a.Kinds)
		if err != nil {
			return "", err
		}
		return jsonString(out)
	case "search_elements":
		var a struct {
			Query        string `json:"query"`
			Kind         string `json:"kind"`
			RFC2119Level string `json:"rfc2119_level"`
			Document     string `json:"document"`
			Topic        string `json:"topic"`
			Limit        int    `json:"limit"`
			Cursor       string `json:"cursor"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
		}
		return jsonString(s.searchElements(a.Query, a.Kind, a.RFC2119Level, a.Document, a.Topic, a.Limit, a.Cursor))
	case "list_taxonomy":
		var a struct {
			Category string `json:"category"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
		}
		if a.Category == "" {
			return jsonString(s.taxonomy)
		}
		section, ok := s.taxonomy[a.Category]
		if !ok {
			return "", fmt.Errorf("unknown taxonomy category: %s", a.Category)
		}
		return jsonString(map[string]any{a.Category: section})
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func jsonString(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ─── Entry point ─────────────────────────────────────────────────────────

func main() {
	corpusDir := flag.String("corpus", ".", "Path to the ietf-corpus repo root.")
	flag.Parse()

	abs, err := filepath.Abs(*corpusDir)
	if err != nil {
		log.Fatal(err)
	}
	s, err := loadStore(abs)
	if err != nil {
		log.Fatal(err)
	}

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	for {
		line, err := in.ReadString('\n')
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("read: %v", err)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			log.Printf("parse: %v", err)
			continue
		}
		resp := s.handle(req)
		if resp.JSONRPC == "" {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			log.Printf("write: %v", err)
			return
		}
		if err := out.Flush(); err != nil {
			log.Printf("flush: %v", err)
			return
		}
	}
}
