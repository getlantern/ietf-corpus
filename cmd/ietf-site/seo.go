package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	siteURL    = "https://ietf.lantern.io/"
	siteName   = "IETF corpus"
	repoURL    = "https://github.com/getlantern/ietf-corpus"
	orgID      = "https://lantern.io/#organization"
	websiteID  = siteURL + "#website"
	datasetID  = siteURL + "#dataset"
	ogImageURL = siteURL + "og.png"
	ogImageAlt = "IETF corpus: every RFC and active Internet-Draft as structured, LLM-callable records"
	cc0URL     = "https://creativecommons.org/publicdomain/zero/1.0/"
	ccBy4URL   = "https://creativecommons.org/licenses/by/4.0/"
)

// Page carries the per-page <head> metadata into the header template.
type Page struct {
	Title       string
	Description string
	BasePath    string
	// Path is the page's canonical path relative to the site root, in the
	// extensionless form Cloudflare Pages redirects .html URLs to.
	Path    string
	OGType  string
	NoIndex bool
	JSONLD  template.JS
}

func (p Page) URL() string { return siteURL + p.Path }

// canonicalPath maps an output file to the URL Cloudflare Pages serves
// it at: index.html → "", foo.html → foo, rfcs/rfc-1.html → rfcs/rfc-1.
func canonicalPath(rel string) string {
	rel = filepath.ToSlash(rel)
	if rel == "index.html" {
		return ""
	}
	return strings.TrimSuffix(rel, ".html")
}

var wsRE = regexp.MustCompile(`\s+`)

// metaDescription collapses whitespace and cuts s at a word boundary so
// it fits in about max characters.
func metaDescription(s string, max int) string {
	s = strings.TrimSpace(wsRE.ReplaceAllString(s, " "))
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)[:max-1]
	cut := string(r)
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:.-—") + "…"
}

var tagRE = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string {
	return strings.TrimSpace(wsRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(s, "")), " "))
}

// ── Structured data ──────────────────────────────────────────────────────

func organizationNode() map[string]any {
	return map[string]any{
		"@type":     "Organization",
		"@id":       orgID,
		"name":      "Lantern",
		"legalName": "Brave New Software Project, Inc.",
		"url":       "https://lantern.io",
		"sameAs":    []string{"https://github.com/getlantern"},
	}
}

func websiteNode() map[string]any {
	return map[string]any{
		"@type":      "WebSite",
		"@id":        websiteID,
		"url":        siteURL,
		"name":       siteName,
		"publisher":  map[string]any{"@id": orgID},
		"inLanguage": "en",
	}
}

func webPageNode(p Page) map[string]any {
	return map[string]any{
		"@type":       "WebPage",
		"@id":         p.URL() + "#webpage",
		"url":         p.URL(),
		"name":        p.Title,
		"description": p.Description,
		"isPartOf":    map[string]any{"@id": websiteID},
		"inLanguage":  "en",
	}
}

func datasetNode(st *stats, lastmod string) map[string]any {
	return map[string]any{
		"@type":         "Dataset",
		"@id":           datasetID,
		"name":          siteName,
		"alternateName": "ietf-corpus",
		"description": fmt.Sprintf("A structured, LLM-callable corpus of every IETF RFC and active Internet-Draft "+
			"(%s RFCs and %s drafts): canonical metadata, IETF controlled vocabularies, the updates, obsoletes "+
			"and replaces graph, and %s LLM-extracted normative requirements and protocol elements. "+
			"Built from the RFC Editor index and the IETF Datatracker, and exposed over MCP.",
			commas(st.TotalRFCs), commas(st.TotalDrafts), commas(st.Elements)),
		"url":    siteURL,
		"sameAs": repoURL,
		// Records, schema and taxonomy are CC0 (LICENSE); the README puts
		// LLM-extracted elements under CC-BY-4.0.
		"license":             []string{cc0URL, ccBy4URL},
		"isAccessibleForFree": true,
		"creator":             map[string]any{"@id": orgID},
		"publisher":           map[string]any{"@id": orgID},
		"dateModified":        lastmod,
		"inLanguage":          "en",
		"keywords": []string{
			"IETF", "RFC", "Internet-Draft", "MCP", "Model Context Protocol", "LLM",
			"protocol specifications", "normative requirements", "RFC 2119",
		},
		"isBasedOn": []string{
			"https://www.rfc-editor.org/rfc-index.xml",
			"https://datatracker.ietf.org/",
		},
		"distribution": []map[string]any{
			{
				"@type":          "DataDownload",
				"name":           "Full corpus (YAML records, schema and taxonomy)",
				"description":    "Document records, schema and taxonomy are CC0; LLM-extracted elements are CC-BY-4.0 with attribution to the source RFC or draft.",
				"encodingFormat": "application/zip",
				"contentUrl":     repoURL + "/archive/refs/heads/main.zip",
			},
			{
				"@type":          "DataDownload",
				"name":           "Document index (id, title, year, status, stream, area, working group)",
				"encodingFormat": "application/json",
				"contentUrl":     siteURL + "data/index.json",
			},
		},
	}
}

func faqPageNode(items []faqItem, pageURL string) map[string]any {
	qs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		qs = append(qs, map[string]any{
			"@type": "Question",
			"name":  it.Q,
			"acceptedAnswer": map[string]any{
				"@type": "Answer",
				"text":  stripTags(string(it.A)),
			},
		})
	}
	return map[string]any{
		"@type":      "FAQPage",
		"@id":        pageURL + "#faq",
		"isPartOf":   map[string]any{"@id": pageURL + "#webpage"},
		"mainEntity": qs,
	}
}

func jsonLD(nodes ...map[string]any) template.JS {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(map[string]any{"@context": "https://schema.org", "@graph": nodes}); err != nil {
		panic(err)
	}
	return template.JS(strings.TrimSpace(buf.String()))
}

// basicPage builds Page metadata plus the shared Organization / WebSite /
// WebPage graph for a page with no page-specific entity.
func basicPage(rel, basePath, title, desc string) Page {
	p := Page{Title: title, Description: desc, BasePath: basePath, Path: canonicalPath(rel), OGType: "website"}
	p.JSONLD = jsonLD(organizationNode(), websiteNode(), webPageNode(p))
	return p
}

func docPage(d *Document) Page {
	label := d.ID
	if d.Type == "rfc" && d.RFCNumber > 0 {
		label = fmt.Sprintf("RFC %d", d.RFCNumber)
	}
	title := label + ": " + d.Title + " · IETF corpus"
	var desc string
	if d.Abstract != "" {
		desc = metaDescription(d.Abstract, 158)
	} else if d.Type == "rfc" {
		desc = metaDescription(fmt.Sprintf("%s, %s. Metadata, status, and the obsoletes and updates graph in the IETF corpus.", label, d.Title), 158)
	} else {
		desc = metaDescription(fmt.Sprintf("Internet-Draft %s, %s. Metadata, state and related documents in the IETF corpus.", d.ID, d.Title), 158)
	}
	p := Page{Title: title, Description: desc, BasePath: "../", Path: canonicalPath(docPath(d.ID)), OGType: "article"}
	wp := webPageNode(p)
	about := map[string]any{
		"@type":      "CreativeWork",
		"name":       d.Title,
		"identifier": label,
	}
	if d.URL != "" {
		about["url"] = d.URL
	}
	if d.Date != "" {
		about["datePublished"] = d.Date
	}
	wp["about"] = about
	wp["isPartOf"] = []map[string]any{{"@id": websiteID}, {"@id": datasetID}}
	p.JSONLD = jsonLD(organizationNode(), websiteNode(), wp)
	return p
}

// ── Static assets, robots, sitemap ──────────────────────────────────────

func (s *site) writeStatic() error {
	for _, name := range []string{"og.png", "favicon.svg"} {
		raw, err := assets.ReadFile("templates/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.out, name), raw, 0o644); err != nil {
			return err
		}
	}
	robots := "User-agent: *\nAllow: /\n\nSitemap: " + siteURL + "sitemap.xml\n"
	return os.WriteFile(filepath.Join(s.out, "robots.txt"), []byte(robots), 0o644)
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

type urlset struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

func (s *site) writeSitemap() error {
	set := urlset{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, p := range s.pages {
		set.URLs = append(set.URLs, sitemapURL{Loc: siteURL + p, LastMod: s.lastmod})
	}
	out, err := xml.MarshalIndent(set, "", " ")
	if err != nil {
		return err
	}
	out = append([]byte(xml.Header), out...)
	out = append(out, '\n')
	return os.WriteFile(filepath.Join(s.out, "sitemap.xml"), out, 0o644)
}

// siteLastMod is the date of the corpus repo's last commit, so a rebuild
// of the same commit produces the same sitemap. Falls back to today.
func siteLastMod(corpus string) string {
	out, err := exec.Command("git", "-C", corpus, "log", "-1", "--format=%cs").Output()
	if d := strings.TrimSpace(string(out)); err == nil && len(d) == len("2006-01-02") {
		return d
	}
	return time.Now().UTC().Format("2006-01-02")
}

// commas formats n with thousands separators: 12277 → 12,277.
func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
