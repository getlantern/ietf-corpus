// ietf-site renders the IETF corpus as a static website. Mirrors the
// loading code from ietf-mcp but emits HTML instead of serving over
// stdio. Designed for deploy on Cloudflare Pages or GitHub Pages.
//
// Usage:
//
//	ietf-site --corpus . --out dist
//
// Output layout (deterministic, suitable for hosting at root):
//
//	dist/
//	  index.html                  landing
//	  browse.html                 client-side filterable index (12k docs)
//	  taxonomy.html               controlled vocab
//	  rfcs/<id>.html              one per RFC
//	  drafts/<id>.html            one per active draft
//	  data/index.json             id/title/year/etc. for client-side filter
//	  styles.css                  the stylesheet
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed templates/*.html templates/styles.css
var assets embed.FS

type Document struct {
	ID                string   `yaml:"id"`
	Type              string   `yaml:"-"` // set on load
	Title             string   `yaml:"title"`
	Date              string   `yaml:"date,omitempty"`
	Authors           []Author `yaml:"authors,omitempty"`
	Abstract          string   `yaml:"abstract,omitempty"`
	URL               string   `yaml:"url,omitempty"`
	Stream            string   `yaml:"stream,omitempty"`
	Area              string   `yaml:"area,omitempty"`
	WG                string   `yaml:"wg,omitempty"`
	Group             string   `yaml:"group,omitempty"`
	Keywords          []string `yaml:"keywords,omitempty"`
	Topics            []string `yaml:"topics,omitempty"`
	RFCNumber         int      `yaml:"rfc_number,omitempty"`
	CurrentStatus     string   `yaml:"current_status,omitempty"`
	PublicationStatus string   `yaml:"publication_status,omitempty"`
	PageCount         int      `yaml:"page_count,omitempty"`
	Formats           []string `yaml:"formats,omitempty"`
	Draft             string   `yaml:"draft,omitempty"`
	Obsoletes         []string `yaml:"obsoletes,omitempty"`
	ObsoletedBy       []string `yaml:"obsoleted_by,omitempty"`
	Updates           []string `yaml:"updates,omitempty"`
	UpdatedBy         []string `yaml:"updated_by,omitempty"`
	Also              []string `yaml:"also,omitempty"`
	DOI               string   `yaml:"doi,omitempty"`
	ErrataURL         string   `yaml:"errata_url,omitempty"`
	Name              string   `yaml:"name,omitempty"`
	Rev               string   `yaml:"rev,omitempty"`
	StreamState       string   `yaml:"stream_state,omitempty"`
	IntendedStatus    string   `yaml:"intended_status,omitempty"`
	LastUpdated       string   `yaml:"last_updated,omitempty"`
	Expires           string   `yaml:"expires,omitempty"`
	Replaces          []string `yaml:"replaces,omitempty"`
	ReplacedBy        []string `yaml:"replaced_by,omitempty"`
	Pages             int      `yaml:"pages,omitempty"`
	Words             int      `yaml:"words,omitempty"`

	Elements []*Element `yaml:"-"`
	Year     int        `yaml:"-"`
}

type Author struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title,omitempty"`
	Affiliation string `yaml:"affiliation,omitempty"`
}

type Element struct {
	ID           string   `yaml:"-"`
	Document     string   `yaml:"document"`
	Kind         string   `yaml:"kind"`
	Summary      string   `yaml:"summary"`
	Section      string   `yaml:"section,omitempty"`
	RFC2119Level string   `yaml:"rfc2119_level,omitempty"`
	Topics       []string `yaml:"topics,omitempty"`
}

func main() {
	corpus := flag.String("corpus", ".", "Corpus root.")
	out := flag.String("out", "dist", "Output directory.")
	flag.Parse()

	corpusAbs, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}
	outAbs, err := filepath.Abs(*out)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	docs, err := loadDocs(corpusAbs)
	if err != nil {
		log.Fatal(err)
	}
	loadElements(corpusAbs, docs)
	log.Printf("loaded %d documents in %s", len(docs), time.Since(start).Round(time.Millisecond))

	if err := os.MkdirAll(outAbs, 0o755); err != nil {
		log.Fatal(err)
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"join":    strings.Join,
		"lower":   strings.ToLower,
		"docPath": docPath,
		"taxLabel": func(v any) string {
			switch x := v.(type) {
			case string:
				return x
			case map[string]any:
				if l, ok := x["label"].(string); ok {
					note, _ := x["note"].(string)
					if note != "" {
						return l + " — " + note
					}
					return l
				}
			}
			return fmt.Sprintf("%v", v)
		},
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					continue
				}
				m[k] = pairs[i+1]
			}
			return m
		},
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		log.Fatal(err)
	}

	st := &site{tpl: tpl, out: outAbs, corpus: corpusAbs, docs: docs, byID: indexByID(docs)}
	if err := st.renderAll(); err != nil {
		log.Fatal(err)
	}
	log.Printf("site written to %s in %s", outAbs, time.Since(start).Round(time.Millisecond))
}

func indexByID(docs []*Document) map[string]*Document {
	m := make(map[string]*Document, len(docs))
	for _, d := range docs {
		m[d.ID] = d
	}
	return m
}

func loadDocs(corpus string) ([]*Document, error) {
	var docs []*Document
	for _, kind := range []string{"rfcs", "drafts"} {
		dir := filepath.Join(corpus, "corpus", kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		t := strings.TrimSuffix(kind, "s")
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			var d Document
			if err := yaml.Unmarshal(raw, &d); err != nil {
				return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
			}
			d.Type = t
			d.Year = deriveYear(&d)
			docs = append(docs, &d)
		}
	}
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].Year != docs[j].Year {
			return docs[i].Year > docs[j].Year
		}
		return docs[i].ID < docs[j].ID
	})
	return docs, nil
}

func deriveYear(d *Document) int {
	candidates := []string{d.Date, d.LastUpdated, d.Expires}
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

func loadElements(corpus string, docs []*Document) {
	dir := filepath.Join(corpus, "corpus", "elements")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	byID := indexByID(docs)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var el Element
		if err := yaml.Unmarshal(raw, &el); err != nil {
			continue
		}
		el.ID = strings.TrimSuffix(e.Name(), ".yaml")
		if d, ok := byID[el.Document]; ok {
			d.Elements = append(d.Elements, &el)
		}
	}
}

// ── Rendering ────────────────────────────────────────────────────────────

type site struct {
	tpl       *template.Template
	out       string
	corpus    string
	docs      []*Document
	byID      map[string]*Document
}

func (s *site) renderAll() error {
	if err := s.writeStyles(); err != nil {
		return err
	}
	if err := s.renderIndex(); err != nil {
		return err
	}
	if err := s.renderTaxonomy(); err != nil {
		return err
	}
	if err := s.renderBrowse(); err != nil {
		return err
	}
	if err := s.renderDataset(); err != nil {
		return err
	}
	rfcCount, draftCount := 0, 0
	for _, d := range s.docs {
		if err := s.renderDoc(d); err != nil {
			return err
		}
		if d.Type == "rfc" {
			rfcCount++
		} else {
			draftCount++
		}
	}
	log.Printf("rendered %d RFC pages, %d draft pages", rfcCount, draftCount)
	return nil
}

func (s *site) writeStyles() error {
	raw, err := assets.ReadFile("templates/styles.css")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.out, "styles.css"), raw, 0o644)
}

type stats struct {
	TotalRFCs   int
	TotalDrafts int
	Streams     map[string]int
	Areas       map[string]int
	WGs         map[string]int
	Statuses    map[string]int
	WithElements int
	Elements     int
	NewestRFC    *Document
	OldestRFC    *Document
}

func (s *site) buildStats() *stats {
	st := &stats{Streams: map[string]int{}, Areas: map[string]int{}, WGs: map[string]int{}, Statuses: map[string]int{}}
	for _, d := range s.docs {
		switch d.Type {
		case "rfc":
			st.TotalRFCs++
			if d.Stream != "" {
				st.Streams[d.Stream]++
			}
			if d.Area != "" {
				st.Areas[d.Area]++
			}
			if d.WG != "" {
				st.WGs[d.WG]++
			}
			if d.CurrentStatus != "" {
				st.Statuses[d.CurrentStatus]++
			}
			if st.NewestRFC == nil || d.RFCNumber > st.NewestRFC.RFCNumber {
				st.NewestRFC = d
			}
			if st.OldestRFC == nil || (d.RFCNumber > 0 && d.RFCNumber < st.OldestRFC.RFCNumber) {
				st.OldestRFC = d
			}
		case "draft":
			st.TotalDrafts++
		}
		if len(d.Elements) > 0 {
			st.WithElements++
			st.Elements += len(d.Elements)
		}
	}
	return st
}

func (s *site) renderIndex() error {
	st := s.buildStats()
	return s.write("index.html", "index.html", map[string]any{
		"Stats":     st,
		"TopAreas":  topN(st.Areas, 8),
		"TopWGs":    topN(st.WGs, 12),
		"TopStatus": topN(st.Statuses, 6),
		"FeaturedRFCs": []string{
			"rfc-9000", "rfc-8446", "rfc-9114", "rfc-2119", "rfc-791", "rfc-793", "rfc-9110", "rfc-7540",
		},
		"ByID":      s.byID,
	})
}

func (s *site) renderTaxonomy() error {
	raw, err := os.ReadFile(filepath.Join(s.corpus, "schema", "taxonomy.yaml"))
	if err != nil {
		return err
	}
	var t map[string]any
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return err
	}
	return s.write("taxonomy.html", "taxonomy.html", map[string]any{"Taxonomy": t})
}

func (s *site) renderBrowse() error {
	return s.write("browse.html", "browse.html", map[string]any{
		"Total": len(s.docs),
	})
}

func (s *site) renderDataset() error {
	// Slim records for client-side filter — id, title, year, type, area,
	// wg/group, status. Keeping this small is critical: a 50MB blob hurts
	// page-load. Plain JSON, no compression (CF gzips on the wire).
	type slim struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Title  string `json:"title"`
		Year   int    `json:"year,omitempty"`
		Status string `json:"status,omitempty"`
		Stream string `json:"stream,omitempty"`
		Area   string `json:"area,omitempty"`
		WG     string `json:"wg,omitempty"`
	}
	rows := make([]slim, 0, len(s.docs))
	for _, d := range s.docs {
		row := slim{
			ID: d.ID, Type: d.Type, Title: d.Title, Year: d.Year,
			Stream: d.Stream, Area: d.Area,
		}
		if d.Type == "rfc" {
			row.Status = d.CurrentStatus
			row.WG = d.WG
		} else {
			row.Status = d.StreamState
			row.WG = d.Group
		}
		rows = append(rows, row)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.out, "data"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.out, "data", "index.json"), data, 0o644)
}

func (s *site) renderDoc(d *Document) error {
	rel := docPath(d.ID)
	related := s.expandRelated(d)
	return s.write(rel, "document.html", map[string]any{
		"D":       d,
		"Related": related,
	})
}

type relatedSet struct {
	Obsoletes        []*Document
	ObsoletedBy      []*Document
	Updates          []*Document
	UpdatedBy        []*Document
	Also             []string
	Replaces         []*Document
	ReplacedBy       []*Document
	PredecessorDraft *Document
	SuccessorRFC     *Document
}

func (s *site) expandRelated(d *Document) *relatedSet {
	r := &relatedSet{Also: d.Also}
	lookup := func(ids []string) []*Document {
		var out []*Document
		for _, id := range ids {
			if got, ok := s.byID[id]; ok {
				out = append(out, got)
			}
		}
		return out
	}
	r.Obsoletes = lookup(d.Obsoletes)
	r.ObsoletedBy = lookup(d.ObsoletedBy)
	r.Updates = lookup(d.Updates)
	r.UpdatedBy = lookup(d.UpdatedBy)
	r.Replaces = lookup(d.Replaces)
	r.ReplacedBy = lookup(d.ReplacedBy)
	if d.Type == "rfc" && d.Draft != "" {
		if got, ok := s.byID[stripDraftRev(d.Draft)]; ok {
			r.PredecessorDraft = got
		}
	}
	if d.Type == "draft" && d.RFCNumber > 0 {
		if got, ok := s.byID[fmt.Sprintf("rfc-%d", d.RFCNumber)]; ok {
			r.SuccessorRFC = got
		}
	}
	return r
}

func (s *site) write(rel, tplName string, data any) error {
	full := filepath.Join(s.out, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.tpl.ExecuteTemplate(f, tplName, data)
}

func docPath(id string) string {
	switch {
	case strings.HasPrefix(id, "rfc-"):
		return filepath.Join("rfcs", id+".html")
	case strings.HasPrefix(id, "draft-"):
		return filepath.Join("drafts", id+".html")
	}
	return id + ".html"
}

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

type kv struct {
	K string
	V int
}

func topN(m map[string]int, n int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].V != out[j].V {
			return out[i].V > out[j].V
		}
		return out[i].K < out[j].K
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
