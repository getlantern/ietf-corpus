package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCanonicalPath(t *testing.T) {
	for in, want := range map[string]string{
		"index.html":          "",
		"browse.html":         "browse",
		"rfcs/rfc-9000.html":  "rfcs/rfc-9000",
		"drafts/draft-x.html": "drafts/draft-x",
	} {
		if got := canonicalPath(in); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetaDescription(t *testing.T) {
	short := "A  short\n abstract."
	if got := metaDescription(short, 158); got != "A short abstract." {
		t.Errorf("short: got %q", got)
	}
	long := strings.Repeat("word ", 60)
	got := metaDescription(long, 158)
	if n := utf8.RuneCountInString(got); n > 158 {
		t.Errorf("long: %d runes, want <= 158", n)
	}
	if !strings.HasSuffix(got, "word…") {
		t.Errorf("long: should cut at a word boundary, got %q", got)
	}
}

func TestCommas(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 12277: "12,277", 110530: "110,530", 1234567: "1,234,567"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFAQ(t *testing.T) {
	items := faqItems(&stats{TotalRFCs: 1, TotalDrafts: 2, Elements: 3, WithElements: 4})
	if len(items) < 8 || len(items) > 14 {
		t.Fatalf("got %d FAQ items, want 8–14", len(items))
	}
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.Slug()] {
			t.Errorf("duplicate FAQ slug %q", it.Slug())
		}
		seen[it.Slug()] = true
	}

	var graph struct {
		Graph []struct {
			Type       string `json:"@type"`
			MainEntity []struct {
				Name   string `json:"name"`
				Answer struct {
					Text string `json:"text"`
				} `json:"acceptedAnswer"`
			} `json:"mainEntity"`
		} `json:"@graph"`
	}
	raw := jsonLD(organizationNode(), faqPageNode(items, siteURL))
	if err := json.Unmarshal([]byte(raw), &graph); err != nil {
		t.Fatalf("JSON-LD doesn't parse: %v", err)
	}
	faq := graph.Graph[1]
	if faq.Type != "FAQPage" || len(faq.MainEntity) != len(items) {
		t.Fatalf("FAQPage has %d questions, want %d", len(faq.MainEntity), len(items))
	}
	for i, q := range faq.MainEntity {
		if q.Name != items[i].Q {
			t.Errorf("question %d: %q != %q", i, q.Name, items[i].Q)
		}
		if strings.ContainsAny(q.Answer.Text, "<>") {
			t.Errorf("answer %d still has markup: %q", i, q.Answer.Text)
		}
	}
}

func TestDocPage(t *testing.T) {
	p := docPage(&Document{ID: "rfc-9000", Type: "rfc", RFCNumber: 9000, Title: "QUIC", Abstract: "Defines QUIC."})
	if p.URL() != "https://ietf.lantern.io/rfcs/rfc-9000" {
		t.Errorf("URL = %q", p.URL())
	}
	if !strings.HasPrefix(p.Title, "RFC 9000: QUIC") {
		t.Errorf("Title = %q", p.Title)
	}
	var v any
	if err := json.Unmarshal([]byte(p.JSONLD), &v); err != nil {
		t.Errorf("JSON-LD doesn't parse: %v", err)
	}
}
