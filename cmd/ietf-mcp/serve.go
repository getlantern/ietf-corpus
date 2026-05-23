// HTTP serve mode for ietf-mcp.
//
// Endpoints:
//   GET  /healthz         — liveness, returns 200 with corpus stats
//   POST /ask             — natural-language Q&A over the corpus
//   POST /mcp             — MCP over HTTP (JSON-RPC, single-shot)
//
// The /ask flow mirrors corpus-crawl's /ask in circumvention-corpus:
//   1. Look up the question against the local store's elements + docs
//   2. Build a structured prompt (question + matched material + taxonomy)
//   3. Shell out to `claude -p` (auth comes from the user's keychain;
//      this process must therefore run as a LaunchAgent in the user's
//      GUI session, not a system daemon — see deploy/README.md)
//   4. Return {question, answer, bundle, elapsed_ms}
//
// SSE streaming is not implemented in v1. Plain JSON only. Clients
// that need streaming can poll via the MCP for the bundle and call
// claude themselves; the value-add of /ask is the one-call convenience.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	askClaudeModel   = "claude-sonnet-4-6"
	askMaxQuestion   = 500
	askDefaultLimit  = 30
	askMaxLimit      = 60
	askClaudeTimeout = 90 * time.Second
)

// runHTTPServer wires the corpus store into HTTP handlers.
func runHTTPServer(s *store, addr, token string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		n := len(s.docs)
		nElem := len(s.elements)
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"documents":%d,"elements":%d}`, n, nElem)
	})

	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		askHandler(w, r, s, token)
	})

	// /mcp — MCP over HTTP. Bare-bones; single request/response per
	// HTTP call. Useful for clients that prefer HTTP transport over
	// stdio. Auth: same Bearer token as /ask.
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		mcpHTTPHandler(w, r, s, token)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("ietf-mcp: HTTP serve listening on %s (docs=%d, elements=%d)",
		addr, len(s.docs), len(s.elements))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// ── /ask ──────────────────────────────────────────────────────────────

type askRequest struct {
	Question string `json:"question"`
	Limit    int    `json:"limit,omitempty"`
}

type askResponse struct {
	Question string        `json:"question"`
	Answer   string        `json:"answer"`
	Bundle   *askBundle    `json:"bundle"`
	Elapsed  string        `json:"elapsed_ms"`
}

// askBundle is the structured material the LLM saw. Surfacing it back
// to the caller gives the UI provenance ("Claude was shown these
// records") without re-querying.
type askBundle struct {
	Elements  []*Element  `json:"elements"`
	Documents []*Document `json:"documents"`
}

func askHandler(w http.ResponseWriter, r *http.Request, s *store, authToken string) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()

	var req askRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	q := strings.TrimSpace(req.Question)
	if q == "" {
		http.Error(w, "question required", http.StatusBadRequest)
		return
	}
	if len(q) > askMaxQuestion {
		http.Error(w, fmt.Sprintf("question too long (max %d chars)", askMaxQuestion), http.StatusBadRequest)
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = askDefaultLimit
	}
	if limit > askMaxLimit {
		limit = askMaxLimit
	}

	ctx, cancel := context.WithTimeout(r.Context(), askClaudeTimeout+15*time.Second)
	defer cancel()

	start := time.Now()
	bundle := buildAskBundle(s, q, limit)

	// Short-circuit when retrieval found nothing — saves a claude call
	// and gives the user a clearer error than "Claude says it doesn't
	// know".
	if len(bundle.Elements) == 0 && len(bundle.Documents) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(askResponse{
			Question: q,
			Answer:   "No documents or elements in the corpus match this question yet. Try broader keywords, or browse the corpus directly.",
			Bundle:   bundle,
			Elapsed:  time.Since(start).Round(time.Millisecond).String(),
		})
		return
	}

	prompt := formatAskPrompt(q, bundle)
	answer, err := runClaudeAsk(ctx, prompt)
	if err != nil {
		log.Printf("/ask claude: %v", err)
		http.Error(w, "claude execution failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(askResponse{
		Question: q,
		Answer:   strings.TrimSpace(answer),
		Bundle:   bundle,
		Elapsed:  time.Since(start).Round(time.Millisecond).String(),
	})
}

// buildAskBundle gathers the retrieval set for the LLM. Tokenizes
// the question into useful keywords (drops stop words + short tokens),
// runs the existing substring search for each keyword, and unions
// the results scored by how many keywords each record matched.
// Elements (dense normative/protocol detail) get retrieved first;
// matched documents fill in context.
//
// This is intentionally crude — no embeddings, no synonyms, no
// stemming. For v1 it's enough: the corpus haystack already includes
// titles, abstracts, keywords, topics, WG, area, stream, so any
// content-bearing keyword that survives stop-word filtering tends
// to find its targets.
func buildAskBundle(s *store, question string, limit int) *askBundle {
	keywords := tokenizeQuestion(question)
	if len(keywords) == 0 {
		return &askBundle{}
	}

	// Score elements: count distinct keyword hits per element.
	elemScore := map[*Element]int{}
	for _, kw := range keywords {
		hits := s.searchElements(kw, "", "", "", "", 200, "")
		for _, e := range hits.Results {
			elemScore[e]++
		}
	}
	elemsRanked := topByScore(elemScore, limit)

	// Same for documents (smaller cap).
	docScore := map[*Document]int{}
	for _, kw := range keywords {
		hits := s.search(searchArgs{Query: kw, Limit: 100})
		for _, d := range hits.Results {
			docScore[d]++
		}
	}
	docsRanked := topDocsByScore(docScore, limit/2)

	return &askBundle{
		Elements:  elemsRanked,
		Documents: docsRanked,
	}
}

// tokenizeQuestion produces a list of search-worthy keywords from a
// free-form question. Lowercases, splits on non-alphanumerics, drops
// stop words and tokens shorter than 3 chars. Deduplicates while
// preserving order. Keeps anchored tokens like "rfc-9000" intact by
// pre-pass before the alpha-num split.
func tokenizeQuestion(q string) []string {
	q = strings.ToLower(q)
	var out []string
	seen := map[string]bool{}

	// Preserve rfc-N, draft-..., bcp-N, std-N — split on whitespace first
	// and check each whitespace token for these patterns before the
	// stricter alnum tokenizer eats their hyphens.
	for _, ws := range strings.Fields(q) {
		ws = strings.Trim(ws, ".,;:?!()[]\"'")
		if isAnchoredID(ws) && !seen[ws] {
			seen[ws] = true
			out = append(out, ws)
		}
	}

	// Now general alnum tokens.
	var buf strings.Builder
	flush := func() {
		t := buf.String()
		buf.Reset()
		if len(t) < 3 {
			return
		}
		if askStopWords[t] {
			return
		}
		if seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, r := range q {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func isAnchoredID(s string) bool {
	return strings.HasPrefix(s, "rfc-") ||
		strings.HasPrefix(s, "draft-") ||
		strings.HasPrefix(s, "bcp-") ||
		strings.HasPrefix(s, "std-") ||
		strings.HasPrefix(s, "fyi-")
}

// English stop words — small handcrafted set sufficient for IETF
// question phrasings. Not a full NLP toolkit; just stripping the most
// common noise so a 7-word question doesn't drown out the signal.
var askStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "are": true,
	"this": true, "that": true, "from": true, "have": true, "has": true,
	"can": true, "you": true, "what": true, "when": true, "where": true,
	"who": true, "why": true, "how": true, "does": true, "did": true,
	"will": true, "would": true, "could": true, "should": true, "may": true,
	"is": true, "be": true, "of": true, "in": true, "to": true, "on": true,
	"at": true, "by": true, "as": true, "an": true, "or": true, "if": true,
	"it": true, "its": true, "but": true, "not": true, "no": true,
	"any": true, "all": true, "some": true, "which": true, "than": true,
	"there": true, "here": true, "between": true, "into": true, "about": true,
	"over": true, "under": true, "out": true, "use": true, "used": true,
	"using": true, "also": true, "more": true, "most": true, "much": true,
	"like": true, "such": true, "rfc": true, "draft": true, // these are too generic; specific ids are caught by isAnchoredID
}

func topByScore(m map[*Element]int, limit int) []*Element {
	type kv struct {
		e *Element
		s int
	}
	kvs := make([]kv, 0, len(m))
	for e, s := range m {
		kvs = append(kvs, kv{e, s})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].s != kvs[j].s {
			return kvs[i].s > kvs[j].s
		}
		return kvs[i].e.ID < kvs[j].e.ID
	})
	if len(kvs) > limit {
		kvs = kvs[:limit]
	}
	out := make([]*Element, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.e
	}
	return out
}

func topDocsByScore(m map[*Document]int, limit int) []*Document {
	type kv struct {
		d *Document
		s int
	}
	kvs := make([]kv, 0, len(m))
	for d, s := range m {
		kvs = append(kvs, kv{d, s})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].s != kvs[j].s {
			return kvs[i].s > kvs[j].s
		}
		return kvs[i].d.ID < kvs[j].d.ID
	})
	if len(kvs) > limit {
		kvs = kvs[:limit]
	}
	out := make([]*Document, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.d
	}
	return out
}

// formatAskPrompt builds the structured prompt sent to claude. The
// prompt explicitly asks for citations of the form (rfc-N §S) so the
// model surfaces its provenance.
func formatAskPrompt(q string, b *askBundle) string {
	var sb strings.Builder
	sb.WriteString("You are answering a question about the IETF corpus (RFCs and Internet-Drafts) using a small set of retrieved records.\n\n")
	sb.WriteString("Answer the question concisely, in two to five sentences. CITE specific RFC numbers and section references from the retrieved material — format citations as `(rfc-9000 §17.2)` or `(draft-ietf-tls-esni)`. If the retrieved material doesn't fully answer the question, say so directly; don't invent facts.\n\n")
	sb.WriteString("QUESTION:\n")
	sb.WriteString(q)
	sb.WriteString("\n\n")

	if len(b.Elements) > 0 {
		// Stable order: by document id then section, so the model
		// reads obsoletes/updates pairs together.
		sorted := append([]*Element{}, b.Elements...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Document != sorted[j].Document {
				return sorted[i].Document < sorted[j].Document
			}
			return sorted[i].Section < sorted[j].Section
		})
		sb.WriteString("RETRIEVED ELEMENTS (LLM-extracted normative requirements, protocol elements, considerations, design rationale):\n")
		for _, e := range sorted {
			sb.WriteString("- ")
			sb.WriteString(e.Document)
			if e.Section != "" {
				sb.WriteString(" §")
				sb.WriteString(e.Section)
			}
			sb.WriteString(" [")
			sb.WriteString(e.Kind)
			if e.RFC2119Level != "" {
				sb.WriteString("/")
				sb.WriteString(e.RFC2119Level)
			}
			sb.WriteString("] ")
			sb.WriteString(strings.TrimSpace(e.Summary))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if len(b.Documents) > 0 {
		sb.WriteString("RETRIEVED DOCUMENTS (title + abstract, for surrounding context):\n")
		for _, d := range b.Documents {
			sb.WriteString("- ")
			sb.WriteString(d.ID)
			sb.WriteString(" ")
			sb.WriteString(d.Title)
			if d.Stream != "" || d.CurrentStatus != "" || d.WG != "" {
				sb.WriteString(" [")
				parts := []string{}
				if d.Stream != "" {
					parts = append(parts, d.Stream)
				}
				if d.CurrentStatus != "" {
					parts = append(parts, d.CurrentStatus)
				}
				if d.WG != "" {
					parts = append(parts, "wg="+d.WG)
				}
				sb.WriteString(strings.Join(parts, ", "))
				sb.WriteString("]")
			}
			if d.Abstract != "" {
				abs := strings.TrimSpace(d.Abstract)
				if len(abs) > 400 {
					abs = abs[:400] + "..."
				}
				sb.WriteString("\n  ")
				sb.WriteString(abs)
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\nNow answer the question. Cite specific records using (rfc-N §S) or (draft-name) format.\n")
	return sb.String()
}

// runClaudeAsk invokes `claude -p` and extracts the answer. Output
// format is JSON; the inner "result" field carries the model's reply.
// Errors include both stdout and stderr so quota / auth / network
// failures are diagnosable from a single log line (same lesson as
// ietf-elements: don't lose the actual claude error to stderr-was-empty).
func runClaudeAsk(ctx context.Context, prompt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, askClaudeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "-p", "--output-format", "json", "--model", askClaudeModel, prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		so := strings.TrimSpace(stdout.String())
		se := strings.TrimSpace(stderr.String())
		if len(so) > 400 {
			so = so[:400] + "...[truncated]"
		}
		if len(se) > 400 {
			se = se[:400] + "...[truncated]"
		}
		return "", fmt.Errorf("claude -p: %w (stdout=%q stderr=%q)", err, so, se)
	}
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err == nil {
		if env.IsError {
			return "", fmt.Errorf("claude returned error: %s", env.Result)
		}
		if env.Result != "" {
			return env.Result, nil
		}
	}
	// Fallback for non-envelope responses.
	return stdout.String(), nil
}

// ── /mcp ──────────────────────────────────────────────────────────────

// mcpHTTPHandler exposes the same JSON-RPC dispatch the stdio mode
// uses, but over a single-shot HTTP POST. Useful for clients that
// can't manage a long-lived subprocess.
func mcpHTTPHandler(w http.ResponseWriter, r *http.Request, s *store, authToken string) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp := s.handle(req)
	w.Header().Set("Content-Type", "application/json")
	if resp.JSONRPC == "" {
		// notifications/initialized — no response per MCP convention.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}
