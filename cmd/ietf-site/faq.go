package main

import (
	"fmt"
	"html/template"
	"regexp"
	"strings"
)

// faqItem is one question on the landing page. The visible FAQ and its
// FAQPage JSON-LD are both rendered from faqItems, so they can't drift.
type faqItem struct {
	Q string
	A template.HTML
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func (f faqItem) Slug() string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(f.Q), "-"), "-")
}

func faqItems(st *stats) []faqItem {
	return []faqItem{
		{
			Q: "What is the IETF corpus?",
			A: template.HTML(fmt.Sprintf(`The IETF corpus (ietf-corpus) is a structured, LLM-callable index of every IETF RFC and every active Internet-Draft, from <a href="https://lantern.io">Lantern</a>. Each document is a YAML record with its metadata (title, authors, date, stream, area, working group and status) and its updates, obsoletes and replaces links. A growing number also carry LLM-extracted normative requirements and protocol elements. It currently holds %s RFCs and %s Internet-Drafts, and an MCP server lets any AI agent query it.`, commas(st.TotalRFCs), commas(st.TotalDrafts))),
		},
		{
			Q: "Who makes the IETF corpus?",
			A: `<a href="https://lantern.io">Lantern</a>, which also makes a free censorship-circumvention app of the same name. The corpus is general-purpose: it has no Lantern-specific tags or relevance scoring, only IETF metadata plus topic tags anyone can extend. The code and data are on <a href="https://github.com/getlantern/ietf-corpus">GitHub</a>.`,
		},
		{
			Q: "How do I use the IETF corpus from Claude Code or another MCP client?",
			A: `Install the <code>ietf-mcp</code> server with <code>go install github.com/getlantern/ietf-corpus/cmd/ietf-mcp@latest</code>, clone the repository, and register the server with your client. In Claude Code that's <code>claude mcp add -s user ietf-corpus $(go env GOPATH)/bin/ietf-mcp -- --corpus $HOME/code/ietf-corpus</code>. The server reads the YAML records from your clone.`,
		},
		{
			Q: "What tools does the IETF corpus MCP server provide?",
			A: `Six: <code>search_documents</code> (free-text and filtered search, cursor-paginated), <code>get_document</code> and <code>get_documents</code> (fetch one or a batch of records by id), <code>find_related</code> (follow obsoletes, updates, replaces and the draft-to-RFC links), <code>search_elements</code> (search the extracted requirements and protocol elements) and <code>list_taxonomy</code> (the controlled vocabularies). They're small and composable, so they work well with code-mode agents.`,
		},
		{
			Q: "How is it different from rfc-editor.org and the IETF Datatracker?",
			A: `Those are the authoritative sources, and the corpus is built from them: RFC records come from the RFC Editor's <code>rfc-index.xml</code>, and draft records from the Datatracker API. The corpus doesn't host the documents. It puts their metadata, the relationship graph and the extracted elements in one uniform schema, with tools an LLM can call, and links every record back to its canonical page.`,
		},
		{
			Q: "Does the IETF corpus include the full text of RFCs?",
			A: `No. It's a research index, not a document host, and doesn't redistribute the documents. Each record points at the canonical copy on rfc-editor.org or datatracker.ietf.org.`,
		},
		{
			Q: "What are extracted elements?",
			A: template.HTML(fmt.Sprintf(`Short structured records an LLM pulls out of an RFC's text, each tied to a section: normative requirements with their RFC&nbsp;2119 level (MUST, SHOULD, MAY), protocol elements, wire formats, state machines, IANA registries, security and privacy considerations, interoperability notes and design rationale. They're extracted with Claude, mostly from standards-track RFCs. So far there are %s elements across %s documents, and drafts don't have them yet.`, commas(st.Elements), commas(st.WithElements))),
		},
		{
			Q: "Which Internet-Drafts are included?",
			A: `Only drafts that were active as of the last crawl. When the crawler syncs, it removes drafts that have expired, been replaced or been published as an RFC. Drafts are identified by their base name without the revision number, such as <code>draft-ietf-tls-esni</code>, and RFCs as <code>rfc-</code> plus the number, such as <code>rfc-9000</code>.`,
		},
		{
			Q: "Can I ask the IETF corpus questions in plain English?",
			A: `Yes. The <a href="ask.html">ask</a> page answers a natural-language question with Claude, using requirements and documents found in the corpus, and cites the RFCs and sections it relied on. It's rate-limited per IP address. For heavier use, connect the MCP server to your own agent.`,
		},
		{
			Q: "Can I browse the corpus without an AI agent?",
			A: `Yes. The <a href="browse.html">browse</a> page filters every document by title, id, working group, area, status and type in your browser. Each RFC and draft has its own page with its metadata, abstract, related documents and any extracted elements, and the <a href="taxonomy.html">taxonomy</a> page lists the controlled vocabularies.`,
		},
		{
			Q: "What license is the IETF corpus under?",
			A: `The schema, taxonomy and document records are released under CC0, into the public domain. The project README says LLM-extracted elements are CC-BY-4.0, with attribution to the source RFC or draft. The RFCs and drafts themselves remain under the <a href="https://trustee.ietf.org/license-info/">IETF Trust Legal Provisions</a>.`,
		},
		{
			Q: "Can I run the IETF corpus tools myself?",
			A: `Yes. The code is in the <a href="https://github.com/getlantern/ietf-corpus">repository</a>, and the command-line tools are written in Go: <code>go run ./cmd/ietf-crawl --corpus .</code> refreshes the RFC and draft records, <code>ietf-mcp</code> serves them over MCP, and <code>go run ./cmd/ietf-site --corpus . --out dist</code> renders this website.`,
		},
	}
}
