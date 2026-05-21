package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadTestStore(t *testing.T) *store {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadStore(root)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if len(s.docs) == 0 {
		t.Fatal("no documents loaded")
	}
	return s
}

// TestSearchByQuery confirms substring search works against title + abstract.
func TestSearchByQuery(t *testing.T) {
	s := loadTestStore(t)
	resp := s.search(searchArgs{Query: "QUIC", Type: "rfc", Limit: 5})
	if resp.Total < 5 {
		t.Errorf("expected many RFCs matching QUIC, got %d", resp.Total)
	}
	if len(resp.Results) == 0 {
		t.Fatal("no results")
	}
	// The query matches via the haystack (title + abstract + keywords + authors + topics + WG/area).
	// Verify the top result actually contains "quic" in at least one of those fields.
	top := resp.Results[0]
	hay := strings.ToLower(top.Title + " " + top.Abstract + " " + strings.Join(top.Keywords, " ") + " " + top.WG + " " + top.Area)
	if !strings.Contains(hay, "quic") {
		t.Errorf("top result doesn't mention QUIC in any expected field: %+v", top)
	}
}

// TestSearchPagination confirms next_cursor moves the window forward.
func TestSearchPagination(t *testing.T) {
	s := loadTestStore(t)
	page1 := s.search(searchArgs{Type: "rfc", Limit: 10})
	if page1.NextCursor == "" {
		t.Fatalf("expected next_cursor on first page; total=%d", page1.Total)
	}
	page2 := s.search(searchArgs{Type: "rfc", Limit: 10, Cursor: page1.NextCursor})
	if len(page2.Results) == 0 {
		t.Fatal("page 2 empty")
	}
	// Pages must not overlap.
	seen := map[string]bool{}
	for _, d := range page1.Results {
		seen[d.ID] = true
	}
	for _, d := range page2.Results {
		if seen[d.ID] {
			t.Errorf("pagination overlap: %s appears on both pages", d.ID)
		}
	}
}

// TestGetDocument confirms a known RFC round-trips with its known fields.
func TestGetDocument(t *testing.T) {
	s := loadTestStore(t)
	d, ok := s.byID["rfc-9000"]
	if !ok {
		t.Fatal("rfc-9000 not loaded")
	}
	if d.Type != "rfc" || d.RFCNumber != 9000 || d.WG != "quic" {
		t.Errorf("rfc-9000 fields wrong: type=%q num=%d wg=%q", d.Type, d.RFCNumber, d.WG)
	}
	if d.CurrentStatus != "PROPOSED STANDARD" {
		t.Errorf("rfc-9000 status: %q", d.CurrentStatus)
	}
}

// TestFindRelatedObsoletion exercises the obsoletes graph.
func TestFindRelatedObsoletion(t *testing.T) {
	s := loadTestStore(t)
	// rfc-8446 (TLS 1.3) obsoletes rfc-5246 (TLS 1.2).
	r, err := s.findRelated("rfc-8446", []string{"obsoletes"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, d := range r.Relations["obsoletes"] {
		ids = append(ids, d.ID)
	}
	if !slices.Contains(ids, "rfc-5246") {
		t.Errorf("rfc-8446.obsoletes missing rfc-5246; got %v", ids)
	}
	// Reverse edge.
	r2, _ := s.findRelated("rfc-5246", []string{"obsoleted_by"})
	ids = ids[:0]
	for _, d := range r2.Relations["obsoleted_by"] {
		ids = append(ids, d.ID)
	}
	if !slices.Contains(ids, "rfc-8446") {
		t.Errorf("rfc-5246.obsoleted_by missing rfc-8446; got %v", ids)
	}
}

// TestFindRelatedAliases confirms BCP/STD aliases are surfaced as stub
// documents (since we don't load BCP/STD records themselves).
func TestFindRelatedAliases(t *testing.T) {
	s := loadTestStore(t)
	r, err := s.findRelated("rfc-2119", []string{"also"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, d := range r.Relations["also"] {
		ids = append(ids, d.ID)
	}
	if !slices.Contains(ids, "bcp-14") {
		t.Errorf("rfc-2119.also missing bcp-14; got %v", ids)
	}
}

// TestGetDocumentsBatch is the code-mode batch idiom.
func TestGetDocumentsBatch(t *testing.T) {
	s := loadTestStore(t)
	raw := json.RawMessage(`{"ids":["rfc-9000","rfc-8446","rfc-does-not-exist"]}`)
	out, err := s.callTool("get_documents", raw)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Documents map[string]*Document `json:"documents"`
		NotFound  []string             `json:"not_found"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Documents) != 2 {
		t.Errorf("expected 2 found docs, got %d", len(got.Documents))
	}
	if !slices.Contains(got.NotFound, "rfc-does-not-exist") {
		t.Errorf("expected rfc-does-not-exist in not_found; got %v", got.NotFound)
	}
}

// TestSearchFilters exercises structured filters.
func TestSearchFilters(t *testing.T) {
	s := loadTestStore(t)
	// WG filter: every TLS WG document should have wg=tls or group=tls.
	resp := s.search(searchArgs{WG: "tls", Limit: 200})
	if resp.Total < 50 {
		t.Errorf("expected >50 TLS WG docs, got %d", resp.Total)
	}
	for _, d := range resp.Results {
		if !strings.EqualFold(d.WG, "tls") && !strings.EqualFold(d.Group, "tls") {
			t.Errorf("WG filter returned non-TLS doc: %s wg=%q group=%q", d.ID, d.WG, d.Group)
		}
	}
	// Status filter: PROPOSED STANDARD matches RFCs via current_status
	// or drafts via intended_status. Either is a valid hit.
	resp = s.search(searchArgs{Status: "PROPOSED STANDARD", Limit: 50})
	if resp.Total < 100 {
		t.Errorf("expected many proposed-standard docs, got %d", resp.Total)
	}
	for _, d := range resp.Results {
		matched := strings.EqualFold(d.CurrentStatus, "PROPOSED STANDARD") ||
			strings.EqualFold(d.IntendedStatus, "PROPOSED STANDARD") ||
			strings.EqualFold(d.StreamState, "PROPOSED STANDARD")
		if !matched {
			t.Errorf("status filter returned doc with no matching status field: %s current=%q intended=%q state=%q",
				d.ID, d.CurrentStatus, d.IntendedStatus, d.StreamState)
		}
	}
}

// TestListTaxonomy confirms the taxonomy is loadable.
func TestListTaxonomy(t *testing.T) {
	s := loadTestStore(t)
	if _, ok := s.taxonomy["streams"]; !ok {
		t.Errorf("taxonomy missing 'streams' section")
	}
	if _, ok := s.taxonomy["topics"]; !ok {
		t.Errorf("taxonomy missing 'topics' section")
	}
}

// TestStripDraftRev covers the helper that maps a versioned draft id
// to the base name used as our YAML id.
func TestStripDraftRev(t *testing.T) {
	cases := map[string]string{
		"draft-ietf-quic-transport-34":  "draft-ietf-quic-transport",
		"draft-ietf-tls-esni-22":        "draft-ietf-tls-esni",
		"draft-bradner-key-words-02":    "draft-bradner-key-words",
		"draft-foo-bar":                 "draft-foo-bar", // no rev suffix
	}
	for in, want := range cases {
		if got := stripDraftRev(in); got != want {
			t.Errorf("stripDraftRev(%q) = %q, want %q", in, got, want)
		}
	}
}
