// ietf-crawl ingests RFC and Internet-Draft metadata into the corpus.
//
// Two sources:
//
//   1. RFCs — rfc-editor.org/rfc-index.xml (the canonical RFC Editor
//      index). One YAML written per RFC to corpus/rfcs/<id>.yaml.
//      BCP/STD/FYI sub-series entries are absorbed into the matching
//      RFC's `also` field.
//
//   2. Internet-Drafts — datatracker.ietf.org JSON API. Reference
//      tables (groups, doc-states, streams, intended-std-levels) are
//      bulk-prefetched once so per-draft URI references resolve from
//      memory. Active drafts only — expired / replaced / rfc states
//      are pruned from corpus/drafts/.
//
// Usage:
//
//	go run ./cmd/ietf-crawl --corpus . [--source rfcs|drafts|all]
//
// Re-running is safe: existing YAMLs are overwritten with the latest
// upstream view. Files for drafts that have left the active state are
// removed.
package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func main() {
	corpus := flag.String("corpus", ".", "Repo root containing schema/ and corpus/.")
	source := flag.String("source", "all", "Which source to ingest: rfcs | drafts | all.")
	rfcIndexURL := flag.String("rfc-index", "https://www.rfc-editor.org/rfc-index.xml", "URL of the RFC Editor index.")
	dtBase := flag.String("datatracker", "https://datatracker.ietf.org", "Datatracker base URL.")
	limit := flag.Int("limit", 0, "If > 0, ingest at most this many records from each source. For smoke-testing.")
	flag.Parse()

	root, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")

	switch *source {
	case "rfcs":
		mustIngestRFCs(root, *rfcIndexURL, today, *limit)
	case "drafts":
		mustIngestDrafts(root, *dtBase, today, *limit)
	case "all":
		mustIngestRFCs(root, *rfcIndexURL, today, *limit)
		mustIngestDrafts(root, *dtBase, today, *limit)
	default:
		log.Fatalf("unknown --source %q (want rfcs|drafts|all)", *source)
	}
}

// ─── RFC ingest ───────────────────────────────────────────────────────────

func mustIngestRFCs(root, indexURL, today string, limit int) {
	log.Printf("rfcs: fetching %s", indexURL)
	raw, err := httpGet(indexURL)
	if err != nil {
		log.Fatalf("rfcs: fetch index: %v", err)
	}
	var idx rfcIndex
	if err := xml.Unmarshal(raw, &idx); err != nil {
		log.Fatalf("rfcs: parse index: %v", err)
	}
	log.Printf("rfcs: index has %d RFCs, %d BCPs, %d STDs, %d FYIs",
		len(idx.RFCs), len(idx.BCPs), len(idx.STDs), len(idx.FYIs))

	// Build RFC→sub-series alias index.
	aliases := map[string][]string{} // rfc-9000 → [bcp-14, std-7]
	collect := func(kind string, entries []subSeriesEntry) {
		for _, e := range entries {
			subID := strings.ToLower(e.DocID) // e.g. "bcp14"
			subID = kind + "-" + strings.TrimPrefix(subID, kind)
			for _, did := range e.IsAlso.DocIDs {
				rfcID := docIDToRFCID(did)
				if rfcID == "" {
					continue
				}
				aliases[rfcID] = append(aliases[rfcID], subID)
			}
		}
	}
	collect("bcp", idx.BCPs)
	collect("std", idx.STDs)
	collect("fyi", idx.FYIs)

	rfcsDir := filepath.Join(root, "corpus", "rfcs")
	if err := os.MkdirAll(rfcsDir, 0o755); err != nil {
		log.Fatal(err)
	}

	written := 0
	for i, r := range idx.RFCs {
		if limit > 0 && written >= limit {
			break
		}
		rec := rfcRecord(r, aliases, today)
		if rec.ID == "" {
			continue
		}
		path := filepath.Join(rfcsDir, rec.ID+".yaml")
		if err := writeYAML(path, rec); err != nil {
			log.Fatalf("rfcs: write %s: %v", path, err)
		}
		written++
		if written%500 == 0 {
			log.Printf("rfcs: wrote %d/%d", written, len(idx.RFCs))
		}
		_ = i
	}
	log.Printf("rfcs: wrote %d records to %s", written, rfcsDir)
}

func rfcRecord(r rfcEntry, aliases map[string][]string, today string) rfcOut {
	id := docIDToRFCID(r.DocID)
	if id == "" {
		return rfcOut{}
	}
	num, _ := strconv.Atoi(strings.TrimPrefix(id, "rfc-"))

	out := rfcOut{
		ID:                id,
		RFCNumber:         num,
		Title:             strings.TrimSpace(r.Title),
		Stream:            r.Stream,
		Area:              strings.ToLower(r.Area),
		WG:                strings.ToLower(r.WGAcronym),
		CurrentStatus:     normalizeStatus(r.CurrentStatus),
		PublicationStatus: normalizeStatus(r.PublicationStatus),
		PageCount:         r.PageCount,
		Draft:             r.Draft,
		DOI:               r.DOI,
		ErrataURL:         r.ErrataURL,
		URL:               fmt.Sprintf("https://www.rfc-editor.org/rfc/rfc%d", num),
		Source:            "rfc-editor-index",
		IngestedAt:        today,
	}

	if r.Date.Month != "" && r.Date.Year != "" {
		out.Date = formatDate(r.Date.Year, r.Date.Month, r.Date.Day)
	}

	for _, a := range r.Authors {
		out.Authors = append(out.Authors, authorOut{Name: a.Name, Title: a.Title})
	}
	for _, f := range r.Format.FileFormats {
		out.Formats = append(out.Formats, f)
	}
	for _, k := range r.Keywords.KWs {
		k = strings.TrimSpace(k)
		if k != "" {
			out.Keywords = append(out.Keywords, k)
		}
	}
	if len(r.Abstract.Paragraphs) > 0 {
		out.Abstract = strings.TrimSpace(strings.Join(r.Abstract.Paragraphs, "\n\n"))
	}

	out.Obsoletes = docIDsToRFCIDs(r.Obsoletes.DocIDs)
	out.ObsoletedBy = docIDsToRFCIDs(r.ObsoletedBy.DocIDs)
	out.Updates = docIDsToRFCIDs(r.Updates.DocIDs)
	out.UpdatedBy = docIDsToRFCIDs(r.UpdatedBy.DocIDs)
	if als := aliases[id]; len(als) > 0 {
		sort.Strings(als)
		out.Also = uniqueStrings(als)
	}
	return out
}

// rfc-editor.org index uses "RFC9000", "BCP14" form. Normalize to
// "rfc-9000", "bcp-14". Returns empty string for unknown doc-id types.
func docIDToRFCID(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "RFC") {
		return ""
	}
	num := strings.TrimPrefix(s, "RFC")
	if _, err := strconv.Atoi(num); err != nil {
		return ""
	}
	n, _ := strconv.Atoi(num)
	return fmt.Sprintf("rfc-%d", n)
}

func docIDsToRFCIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if r := docIDToRFCID(id); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func normalizeStatus(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

var monthNum = map[string]string{
	"January": "01", "February": "02", "March": "03", "April": "04",
	"May": "05", "June": "06", "July": "07", "August": "08",
	"September": "09", "October": "10", "November": "11", "December": "12",
}

func formatDate(year, month, day string) string {
	mn := monthNum[month]
	if mn == "" {
		return ""
	}
	if day == "" {
		return year + "-" + mn
	}
	d, err := strconv.Atoi(day)
	if err != nil {
		return year + "-" + mn
	}
	return fmt.Sprintf("%s-%s-%02d", year, mn, d)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ─── RFC index XML shape ─────────────────────────────────────────────────

type rfcIndex struct {
	XMLName xml.Name           `xml:"rfc-index"`
	BCPs    []subSeriesEntry   `xml:"bcp-entry"`
	STDs    []subSeriesEntry   `xml:"std-entry"`
	FYIs    []subSeriesEntry   `xml:"fyi-entry"`
	RFCs    []rfcEntry         `xml:"rfc-entry"`
}

type subSeriesEntry struct {
	DocID  string `xml:"doc-id"`
	IsAlso struct {
		DocIDs []string `xml:"doc-id"`
	} `xml:"is-also"`
}

type rfcEntry struct {
	DocID   string `xml:"doc-id"`
	Title   string `xml:"title"`
	Authors []struct {
		Name  string `xml:"name"`
		Title string `xml:"title"`
	} `xml:"author"`
	Date struct {
		Month string `xml:"month"`
		Day   string `xml:"day"`
		Year  string `xml:"year"`
	} `xml:"date"`
	Format struct {
		FileFormats []string `xml:"file-format"`
	} `xml:"format"`
	PageCount int `xml:"page-count"`
	Keywords  struct {
		KWs []string `xml:"kw"`
	} `xml:"keywords"`
	Abstract struct {
		Paragraphs []string `xml:"p"`
	} `xml:"abstract"`
	Draft             string `xml:"draft"`
	CurrentStatus     string `xml:"current-status"`
	PublicationStatus string `xml:"publication-status"`
	Stream            string `xml:"stream"`
	Area              string `xml:"area"`
	WGAcronym         string `xml:"wg_acronym"`
	ErrataURL         string `xml:"errata-url"`
	DOI               string `xml:"doi"`
	Obsoletes struct {
		DocIDs []string `xml:"doc-id"`
	} `xml:"obsoletes"`
	ObsoletedBy struct {
		DocIDs []string `xml:"doc-id"`
	} `xml:"obsoleted-by"`
	Updates struct {
		DocIDs []string `xml:"doc-id"`
	} `xml:"updates"`
	UpdatedBy struct {
		DocIDs []string `xml:"doc-id"`
	} `xml:"updated-by"`
}

// ─── Output (YAML) shapes ────────────────────────────────────────────────

type authorOut struct {
	Name  string `yaml:"name"`
	Title string `yaml:"title,omitempty"`
}

type rfcOut struct {
	ID                string      `yaml:"id"`
	RFCNumber         int         `yaml:"rfc_number"`
	Title             string      `yaml:"title"`
	Authors           []authorOut `yaml:"authors,omitempty"`
	Date              string      `yaml:"date,omitempty"`
	Stream            string      `yaml:"stream"`
	Area              string      `yaml:"area,omitempty"`
	WG                string      `yaml:"wg,omitempty"`
	CurrentStatus     string      `yaml:"current_status"`
	PublicationStatus string      `yaml:"publication_status"`
	PageCount         int         `yaml:"page_count,omitempty"`
	Abstract          string      `yaml:"abstract,omitempty"`
	Keywords          []string    `yaml:"keywords,omitempty"`
	Formats           []string    `yaml:"formats,omitempty"`
	Draft             string      `yaml:"draft,omitempty"`
	Obsoletes         []string    `yaml:"obsoletes,omitempty"`
	ObsoletedBy       []string    `yaml:"obsoleted_by,omitempty"`
	Updates           []string    `yaml:"updates,omitempty"`
	UpdatedBy         []string    `yaml:"updated_by,omitempty"`
	Also              []string    `yaml:"also,omitempty"`
	DOI               string      `yaml:"doi,omitempty"`
	ErrataURL         string      `yaml:"errata_url,omitempty"`
	URL               string      `yaml:"url,omitempty"`
	Source            string      `yaml:"source"`
	IngestedAt        string      `yaml:"ingested_at"`
}

type draftAuthorOut struct {
	Name        string `yaml:"name"`
	Email       string `yaml:"email,omitempty"`
	Affiliation string `yaml:"affiliation,omitempty"`
}

type draftOut struct {
	ID              string           `yaml:"id"`
	Name            string           `yaml:"name"`
	Rev             string           `yaml:"rev,omitempty"`
	Title           string           `yaml:"title"`
	Authors         []draftAuthorOut `yaml:"authors,omitempty"`
	Abstract        string           `yaml:"abstract,omitempty"`
	Keywords        []string         `yaml:"keywords,omitempty"`
	Pages           int              `yaml:"pages,omitempty"`
	Words           int              `yaml:"words,omitempty"`
	Stream          string           `yaml:"stream,omitempty"`
	StreamState     string           `yaml:"stream_state"`
	Group           string           `yaml:"group,omitempty"`
	Area            string           `yaml:"area,omitempty"`
	IntendedStatus  string           `yaml:"intended_status,omitempty"`
	FirstSubmitted  string           `yaml:"first_submitted,omitempty"`
	LastUpdated     string           `yaml:"last_updated,omitempty"`
	Expires         string           `yaml:"expires,omitempty"`
	Replaces        []string         `yaml:"replaces,omitempty"`
	ReplacedBy      []string         `yaml:"replaced_by,omitempty"`
	RFCNumber       int              `yaml:"rfc_number,omitempty"`
	URL             string           `yaml:"url,omitempty"`
	Source          string           `yaml:"source"`
	IngestedAt      string           `yaml:"ingested_at"`
}

// ─── Internet-Draft ingest ───────────────────────────────────────────────

// Datatracker returns documents with URI references for stream/group/
// state/std-level. Bulk-prefetch the small reference tables once so
// resolution is in-memory, then page through documents.

type dtListResponse struct {
	Meta struct {
		Limit       int `json:"limit"`
		Offset      int `json:"offset"`
		TotalCount  int `json:"total_count"`
		Next        string `json:"next"`
	} `json:"meta"`
	Objects []json.RawMessage `json:"objects"`
}

type dtDoc struct {
	Name           string   `json:"name"`
	Rev            string   `json:"rev"`
	Title          string   `json:"title"`
	Abstract       string   `json:"abstract"`
	Keywords       string   `json:"keywords"` // JSON array as string, e.g. "[]"
	Pages          int      `json:"pages"`
	Words          int      `json:"words"`
	Stream         string   `json:"stream"`
	Group          string   `json:"group"`
	States         []string `json:"states"`
	IntendedStd    string   `json:"intended_std_level"`
	Expires        string   `json:"expires"`
	Time           string   `json:"time"`
	RFCNumber      *int     `json:"rfc_number"`
	ResourceURI    string   `json:"resource_uri"`
	Type           string   `json:"type"`
}

type dtGroup struct {
	Acronym string `json:"acronym"`
	Name    string `json:"name"`
	Parent  string `json:"parent"`
}

type dtState struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type dtNameSlug struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func mustIngestDrafts(root, dtBase, today string, limit int) {
	log.Printf("drafts: prefetching reference tables from %s", dtBase)

	groups := map[string]dtGroup{}
	// Groups: paginate; ~5k records as of 2026.
	pageJSON(dtBase+"/api/v1/group/group/?limit=500&format=json", func(raw json.RawMessage) error {
		var g dtGroup
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		// Resource URI is needed to key — fetch it from raw.
		var withURI struct {
			ResourceURI string `json:"resource_uri"`
		}
		if err := json.Unmarshal(raw, &withURI); err != nil {
			return err
		}
		groups[withURI.ResourceURI] = g
		return nil
	})
	log.Printf("drafts: loaded %d groups", len(groups))

	// Area parents: a group's parent points at another group; areas have type=area.
	// For day-1 we just expose group acronym; area inference can be added later.

	states := map[string]dtState{}
	pageJSON(dtBase+"/api/v1/doc/state/?limit=500&type=draft&format=json", func(raw json.RawMessage) error {
		var s dtState
		var withURI struct {
			ResourceURI string `json:"resource_uri"`
			Type        string `json:"type"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &withURI); err != nil {
			return err
		}
		states[withURI.ResourceURI] = s
		return nil
	})
	log.Printf("drafts: loaded %d draft states", len(states))

	// Intended-std-level: small enum table.
	stdLevels := map[string]string{}
	pageJSON(dtBase+"/api/v1/name/intendedstdlevelname/?limit=100&format=json", func(raw json.RawMessage) error {
		var n dtNameSlug
		var withURI struct {
			ResourceURI string `json:"resource_uri"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &withURI); err != nil {
			return err
		}
		stdLevels[withURI.ResourceURI] = n.Name
		return nil
	})
	log.Printf("drafts: loaded %d intended-std-levels", len(stdLevels))

	streamSlugs := map[string]string{}
	pageJSON(dtBase+"/api/v1/name/streamname/?limit=100&format=json", func(raw json.RawMessage) error {
		var n dtNameSlug
		var withURI struct {
			ResourceURI string `json:"resource_uri"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &withURI); err != nil {
			return err
		}
		streamSlugs[withURI.ResourceURI] = n.Slug
		return nil
	})
	log.Printf("drafts: loaded %d streams", len(streamSlugs))

	// Now page through active drafts.
	draftsDir := filepath.Join(root, "corpus", "drafts")
	if err := os.MkdirAll(draftsDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// Track which IDs we wrote this run so we can prune drafts that
	// have left the active set.
	keep := map[string]bool{}

	// Filter on expires__gt=<today> too — datatracker leaves many old
	// abandoned drafts in slug=active for years past their expiry,
	// and including them would pollute the corpus with stale records.
	//
	// order_by=id pins a stable sort key so offset-based pagination
	// doesn't double-count or skip records. Without it, the default
	// sort order overlaps adjacent pages by ~10% (verified empirically).
	listURL := fmt.Sprintf("%s/api/v1/doc/document/?type=draft&states__slug=active&expires__gt=%s&order_by=id&limit=200&format=json", dtBase, today)
	written := 0
	pageJSON(listURL, func(raw json.RawMessage) error {
		if limit > 0 && written >= limit {
			return errEarlyStop
		}
		var d dtDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		out := draftRecord(d, groups, states, stdLevels, streamSlugs, today)
		if out.ID == "" {
			return nil
		}
		dst := filepath.Join(draftsDir, out.ID+".yaml")
		if err := writeYAML(dst, out); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		keep[out.ID] = true
		written++
		if written%200 == 0 {
			log.Printf("drafts: wrote %d", written)
		}
		return nil
	})
	log.Printf("drafts: wrote %d active drafts to %s", written, draftsDir)

	// Prune YAMLs whose drafts no longer appear in the active set.
	// Skipped during --limit runs (incomplete pass).
	if limit == 0 {
		entries, err := os.ReadDir(draftsDir)
		if err == nil {
			pruned := 0
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".yaml")
				if !keep[id] {
					if err := os.Remove(filepath.Join(draftsDir, e.Name())); err == nil {
						pruned++
					}
				}
			}
			if pruned > 0 {
				log.Printf("drafts: pruned %d stale records (no longer active)", pruned)
			}
		}
	}
}

func draftRecord(d dtDoc, groups map[string]dtGroup, states map[string]dtState, stdLevels map[string]string, streams map[string]string, today string) draftOut {
	out := draftOut{
		ID:             d.Name,
		Name:           d.Name,
		Rev:            d.Rev,
		Title:          strings.TrimSpace(d.Title),
		Abstract:       strings.TrimSpace(d.Abstract),
		Pages:          d.Pages,
		Words:          d.Words,
		LastUpdated:    truncateToDate(d.Time),
		Expires:        truncateToDate(d.Expires),
		URL:            fmt.Sprintf("https://datatracker.ietf.org/doc/%s/", d.Name),
		Source:         "datatracker",
		IngestedAt:     today,
	}
	if d.RFCNumber != nil && *d.RFCNumber > 0 {
		out.RFCNumber = *d.RFCNumber
	}
	if d.Group != "" {
		if g, ok := groups[d.Group]; ok {
			out.Group = g.Acronym
		}
	}
	if d.Stream != "" {
		if s, ok := streams[d.Stream]; ok {
			out.Stream = canonicalizeStream(s)
		}
	}
	if d.IntendedStd != "" {
		if n, ok := stdLevels[d.IntendedStd]; ok {
			out.IntendedStatus = n
		}
	}
	// Stream state is derived from the draft-type states.
	state := "active"
	for _, sURI := range d.States {
		if s, ok := states[sURI]; ok {
			if s.Slug == "expired" || s.Slug == "replaced" || s.Slug == "rfc" || s.Slug == "withdrawn" || s.Slug == "dead" {
				state = s.Slug
				break
			}
		}
	}
	out.StreamState = state

	// Keywords are JSON-encoded as a string in the document API.
	if d.Keywords != "" && d.Keywords != "[]" {
		var kw []string
		if err := json.Unmarshal([]byte(d.Keywords), &kw); err == nil {
			out.Keywords = kw
		}
	}

	// Hint at canonical group via name structure when API didn't fill
	// it: draft-ietf-quic-transport → group "quic".
	if out.Group == "" {
		out.Group = inferGroupFromName(d.Name)
	}
	return out
}

// inferGroupFromName extracts a likely group acronym from a draft name
// of the form "draft-ietf-<group>-..." or "draft-irtf-<group>-...".
// Returns empty for individual submissions where the second segment
// is an author name, not a group.
func inferGroupFromName(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 4 {
		return ""
	}
	switch parts[1] {
	case "ietf", "irtf", "iab", "editorial":
		return parts[2]
	}
	return ""
}

func canonicalizeStream(slug string) string {
	switch slug {
	case "ietf":
		return "IETF"
	case "iab":
		return "IAB"
	case "irtf":
		return "IRTF"
	case "ise":
		return "Independent"
	case "editorial":
		return "Editorial"
	}
	return slug
}

func truncateToDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

// ─── HTTP helpers ────────────────────────────────────────────────────────

var errEarlyStop = fmt.Errorf("early-stop")

// pageJSON walks a paginated datatracker list endpoint, calling onItem
// for each object. Stops on errEarlyStop. Logs other errors.
func pageJSON(startURL string, onItem func(json.RawMessage) error) {
	next := startURL
	for next != "" {
		// Datatracker returns relative URIs in `next`. Make absolute.
		if strings.HasPrefix(next, "/") {
			next = "https://datatracker.ietf.org" + next
		}
		raw, err := httpGet(next)
		if err != nil {
			log.Printf("page: %v", err)
			return
		}
		var resp dtListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			log.Printf("page: parse: %v", err)
			return
		}
		for _, obj := range resp.Objects {
			if err := onItem(obj); err != nil {
				if err == errEarlyStop {
					return
				}
				log.Printf("page: item: %v", err)
			}
		}
		next = resp.Meta.Next
	}
}

var httpClient = &http.Client{Timeout: 90 * time.Second}

func httpGet(u string) ([]byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ietf-corpus-crawler/0.1 (+https://github.com/getlantern/ietf-corpus)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ─── YAML write ──────────────────────────────────────────────────────────

func writeYAML(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

var _ = path.Join // pacify the import if unused after refactors
