// ietf-elements extracts structured elements (normative requirements,
// protocol elements, security/privacy considerations, design rationale,
// etc.) from IETF RFCs using their canonical plaintext.
//
// Pipeline per RFC:
//
//   1. Read the document YAML at corpus/rfcs/<id>.yaml.
//   2. Fetch the canonical plaintext from rfc-editor.org/rfc/rfc<N>.txt.
//      Cache to corpus/text/<id>.txt.
//   3. Build a structured prompt: taxonomy + document metadata + text +
//      element schema + extraction instructions.
//   4. Call `claude -p --output-format json` with claude-sonnet-4-6.
//   5. Parse the model's JSON array of elements.
//   6. Write one corpus/elements/<id>__<slug>.yaml per element.
//
// Modes:
//
//	ietf-elements extract --id rfc-9000
//	ietf-elements extract-all [--parallel N] [--max N] [--status STATUS]
//
// Drafts aren't supported yet — they churn weekly and pre-extracting
// elements for an expiring document is wasteful. Add when the cost/
// value calculation changes.
//
// Required tools: claude (Claude Code CLI).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	claudeModel     = "claude-sonnet-4-6"
	httpTimeout     = 90 * time.Second
	textCapBytes    = 90_000
	textMinBytes    = 800
	// parallel=1 is the safe default after the May 2026 incident where
	// 3 concurrent claude calls + competing user `claude --continue`
	// sessions on the same machine burned through subscription quota,
	// producing 2,729 failures and 32 successes in 17 hours. Increase
	// only when you know the machine isn't running other claude work.
	defaultParallel = 1
	// Retry policy for transient `claude -p` failures (rate limit,
	// quota recovery, network blip). 3 attempts with quadratic backoff
	// (10s, 40s, 90s) gives the model time to recover without piling
	// retries onto a wedged subscription.
	claudeMaxAttempts = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "extract":
		extractCLI(os.Args[2:])
	case "extract-all":
		extractAllCLI(os.Args[2:])
	case "selftest":
		selftestCLI(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  ietf-elements extract --id rfc-9000 [--corpus PATH] [--force]
  ietf-elements extract-all [--corpus PATH] [--parallel N] [--max N] [--status STATUS] [--area AREA] [--wg WG] [--max-per-hour N] [--force]
  ietf-elements selftest [--corpus PATH] [--ids id1,id2,id3]
`)
	os.Exit(2)
}

func extractCLI(args []string) {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	corpus := fs.String("corpus", ".", "corpus root")
	id := fs.String("id", "", "document id, e.g. rfc-9000")
	force := fs.Bool("force", false, "re-extract even if elements already exist")
	_ = fs.Parse(args)
	if *id == "" {
		fs.Usage()
		os.Exit(2)
	}
	root, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n, err := extract(ctx, root, *id, *force)
	if err != nil {
		log.Fatalf("extract %s: %v", *id, err)
	}
	if n == 0 {
		log.Printf("%s: already has elements (use --force to re-extract)", *id)
	} else {
		log.Printf("%s: wrote %d elements", *id, n)
	}
}

func extractAllCLI(args []string) {
	fs := flag.NewFlagSet("extract-all", flag.ExitOnError)
	corpus := fs.String("corpus", ".", "corpus root")
	parallel := fs.Int("parallel", defaultParallel, "concurrent extractions (default 1 — see runClaude doc)")
	maxN := fs.Int("max", 0, "max documents to process (0 = no limit)")
	status := fs.String("status", "", "filter: only RFCs with this current_status (e.g. 'PROPOSED STANDARD', 'INTERNET STANDARD', 'BEST CURRENT PRACTICE')")
	area := fs.String("area", "", "filter: only RFCs in this area (sec, tsv, art, ...)")
	wg := fs.String("wg", "", "filter: only RFCs from this working group acronym")
	minYear := fs.Int("min-year", 0, "filter: only RFCs published in or after this year")
	maxPerHour := fs.Int("max-per-hour", 0, "cap extractions to this many per rolling hour (0 = no cap). Useful as a quota-burn guard when running unattended.")
	failsExitThreshold := fs.Int("max-consecutive-fails", 10, "after this many back-to-back claude failures, abort the whole sweep. 0 disables — but be careful, the May 2026 incident burned 17 hours unattended at that setting.")
	force := fs.Bool("force", false, "re-extract even if elements already exist")
	_ = fs.Parse(args)
	root, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}

	cands, err := findCandidates(root, *force, candidateFilters{
		Status:  strings.ToUpper(*status),
		Area:    strings.ToLower(*area),
		WG:      strings.ToLower(*wg),
		MinYear: *minYear,
	})
	if err != nil {
		log.Fatal(err)
	}
	if *maxN > 0 && len(cands) > *maxN {
		cands = cands[:*maxN]
	}
	log.Printf("processing %d RFCs with --parallel=%d --max-per-hour=%d", len(cands), *parallel, *maxPerHour)

	rl := newRateLimiter(*maxPerHour)
	abortCtx, abort := context.WithCancel(context.Background())
	defer abort()

	sem := make(chan struct{}, *parallel)
	var wg2 sync.WaitGroup
	var mu sync.Mutex
	var ok, fail, totalElements, consecutiveFails int

	for i, id := range cands {
		if abortCtx.Err() != nil {
			break
		}
		wg2.Add(1)
		sem <- struct{}{}
		go func(idx int, id string) {
			defer wg2.Done()
			defer func() { <-sem }()
			if err := rl.wait(abortCtx); err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(abortCtx, 30*time.Minute) // generous: 3 attempts × 6 min + backoff
			defer cancel()
			n, err := extract(ctx, root, id, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if errors.Is(err, errClaudeAuth) {
					log.Printf("ABORT: %s; tearing down", err)
					abort()
					return
				}
				log.Printf("[%d/%d] FAIL %s: %v", idx+1, len(cands), id, err)
				fail++
				consecutiveFails++
				if *failsExitThreshold > 0 && consecutiveFails >= *failsExitThreshold {
					log.Printf("ABORT: %d consecutive failures hit the --max-consecutive-fails=%d threshold; tearing down", consecutiveFails, *failsExitThreshold)
					abort()
				}
				return
			}
			consecutiveFails = 0
			if n > 0 {
				log.Printf("[%d/%d] ok   %s: %d elements", idx+1, len(cands), id, n)
				ok++
				totalElements += n
			} else {
				log.Printf("[%d/%d] skip %s (already has elements)", idx+1, len(cands), id)
			}
		}(i, id)
	}
	wg2.Wait()
	log.Printf("done: %d ok, %d failed, %d total elements", ok, fail, totalElements)
	if abortCtx.Err() != nil {
		os.Exit(1)
	}
}

// ── rate limiter ──

// rateLimiter caps extractions to maxPerHour in a rolling 1-hour window.
// Pass maxPerHour=0 to disable.
type rateLimiter struct {
	mu         sync.Mutex
	hits       []time.Time
	maxPerHour int
}

func newRateLimiter(maxPerHour int) *rateLimiter {
	return &rateLimiter{maxPerHour: maxPerHour}
}

func (rl *rateLimiter) wait(ctx context.Context) error {
	if rl.maxPerHour <= 0 {
		return nil
	}
	for {
		rl.mu.Lock()
		// GC entries older than 1 hour.
		cutoff := time.Now().Add(-time.Hour)
		keep := rl.hits[:0]
		for _, t := range rl.hits {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		rl.hits = keep
		if len(rl.hits) < rl.maxPerHour {
			rl.hits = append(rl.hits, time.Now())
			rl.mu.Unlock()
			return nil
		}
		// Sleep until the oldest hit ages out of the window.
		oldest := rl.hits[0]
		rl.mu.Unlock()
		wait := time.Until(oldest.Add(time.Hour + time.Second))
		if wait < 0 {
			wait = time.Second
		}
		log.Printf("rate-limit: %d/%d in last hour; sleeping %s", len(rl.hits), rl.maxPerHour, wait.Round(time.Second))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// selftestCLI runs the extractor against a small set of canary RFCs
// to verify everything is working before kicking off a long sweep.
// Exits 0 if all extractions succeed, non-zero if any fail. Designed
// to be the first thing run after deploying / re-enabling the agent
// so quota / network / auth issues surface in minutes, not hours.
//
// Defaults to rfc-7159 (JSON), rfc-3339 (date-time format), and
// rfc-3986 (URIs) — all small Standards-Track RFCs that produce a
// clean set of elements quickly. Override with --ids.
func selftestCLI(args []string) {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	corpus := fs.String("corpus", ".", "corpus root")
	idsFlag := fs.String("ids", "rfc-7159,rfc-3339,rfc-3986", "comma-separated RFC ids to extract")
	_ = fs.Parse(args)
	root, err := filepath.Abs(*corpus)
	if err != nil {
		log.Fatal(err)
	}
	ids := strings.Split(*idsFlag, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	log.Printf("selftest: extracting %d canary RFCs (%s)", len(ids), strings.Join(ids, ", "))
	var failures []string
	for i, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		n, err := extract(ctx, root, id, true) // --force, so existing elements get re-extracted
		cancel()
		if err != nil {
			log.Printf("[%d/%d] FAIL %s: %v", i+1, len(ids), id, err)
			failures = append(failures, id)
			continue
		}
		log.Printf("[%d/%d] ok   %s: %d elements", i+1, len(ids), id, n)
	}
	if len(failures) > 0 {
		log.Printf("selftest FAILED: %d/%d extractions failed (%s)", len(failures), len(ids), strings.Join(failures, ", "))
		os.Exit(1)
	}
	log.Printf("selftest ok: all %d extractions succeeded", len(ids))
}

// ── Candidate selection ──

type candidateFilters struct {
	Status  string
	Area    string
	WG      string
	MinYear int
}

func findCandidates(corpusRoot string, force bool, f candidateFilters) ([]string, error) {
	hasElements := map[string]bool{}
	if !force {
		eDir := filepath.Join(corpusRoot, "corpus", "elements")
		if entries, err := os.ReadDir(eDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
					continue
				}
				id := strings.SplitN(strings.TrimSuffix(e.Name(), ".yaml"), "__", 2)[0]
				hasElements[id] = true
			}
		}
	}

	rDir := filepath.Join(corpusRoot, "corpus", "rfcs")
	entries, err := os.ReadDir(rDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".yaml")
		if hasElements[id] {
			continue
		}
		if f.Status != "" || f.Area != "" || f.WG != "" || f.MinYear > 0 {
			rfc, err := loadRFC(corpusRoot, id)
			if err != nil {
				continue
			}
			if f.Status != "" && !strings.EqualFold(rfc.CurrentStatus, f.Status) {
				continue
			}
			if f.Area != "" && !strings.EqualFold(rfc.Area, f.Area) {
				continue
			}
			if f.WG != "" && !strings.EqualFold(rfc.WG, f.WG) {
				continue
			}
			if f.MinYear > 0 {
				y := rfcYear(rfc)
				if y > 0 && y < f.MinYear {
					continue
				}
			}
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// ── Per-document extraction ──

func extract(ctx context.Context, corpusRoot, id string, force bool) (int, error) {
	if err := requireTool("claude"); err != nil {
		return 0, err
	}

	if !force {
		matches, _ := filepath.Glob(filepath.Join(corpusRoot, "corpus", "elements", id+"__*.yaml"))
		if len(matches) > 0 {
			return 0, nil
		}
	}

	rfc, err := loadRFC(corpusRoot, id)
	if err != nil {
		return 0, fmt.Errorf("load yaml: %w", err)
	}

	txt, err := fetchOrCacheText(ctx, corpusRoot, rfc)
	if err != nil {
		return 0, err
	}
	if len(txt) < textMinBytes {
		return 0, fmt.Errorf("extracted text too short (%d bytes)", len(txt))
	}
	if len(txt) > textCapBytes {
		txt = txt[:textCapBytes] + "\n\n[...truncated for length...]"
	}

	tax, err := os.ReadFile(filepath.Join(corpusRoot, "schema", "taxonomy.yaml"))
	if err != nil {
		return 0, fmt.Errorf("read taxonomy: %w", err)
	}

	prompt := buildPrompt(rfc, string(tax), txt)
	raw, err := runClaude(ctx, prompt)
	if err != nil {
		return 0, fmt.Errorf("claude: %w", err)
	}
	elements, err := parseElements(raw)
	if err != nil {
		return 0, fmt.Errorf("parse model output: %w", err)
	}
	if len(elements) == 0 {
		return 0, errors.New("model returned zero elements")
	}
	written := 0
	for _, e := range elements {
		e.Document = id
		e.ExtractedBy = claudeModel
		e.ExtractedAt = time.Now().UTC().Format("2006-01-02")
		slug := slugifyElement(e)
		if slug == "" {
			continue
		}
		path := filepath.Join(corpusRoot, "corpus", "elements", id+"__"+slug+".yaml")
		if err := writeYAML(path, e); err != nil {
			return written, err
		}
		written++
	}
	log.Printf("ietf-elements: wrote %d elements for %s", written, id)
	return written, nil
}

// ── RFC YAML load ──

type rfcRecord struct {
	ID            string `yaml:"id"`
	RFCNumber     int    `yaml:"rfc_number"`
	Title         string `yaml:"title"`
	Date          string `yaml:"date,omitempty"`
	Abstract      string `yaml:"abstract,omitempty"`
	Stream        string `yaml:"stream,omitempty"`
	Area          string `yaml:"area,omitempty"`
	WG            string `yaml:"wg,omitempty"`
	CurrentStatus string `yaml:"current_status,omitempty"`
	URL           string `yaml:"url,omitempty"`
}

func loadRFC(corpusRoot, id string) (*rfcRecord, error) {
	path := filepath.Join(corpusRoot, "corpus", "rfcs", id+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r rfcRecord
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func rfcYear(r *rfcRecord) int {
	if len(r.Date) < 4 {
		return 0
	}
	var y int
	fmt.Sscanf(r.Date[:4], "%d", &y)
	return y
}

// ── Text fetch + cache ──

func fetchOrCacheText(ctx context.Context, corpusRoot string, r *rfcRecord) (string, error) {
	cachePath := filepath.Join(corpusRoot, "corpus", "text", r.ID+".txt")
	if raw, err := os.ReadFile(cachePath); err == nil {
		return string(raw), nil
	}
	url := fmt.Sprintf("https://www.rfc-editor.org/rfc/rfc%d.txt", r.RFCNumber)
	body, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err == nil {
		_ = os.WriteFile(cachePath, body, 0o644)
	}
	return string(body), nil
}

// ── Prompt + claude invocation ──

func buildPrompt(r *rfcRecord, taxonomy, text string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "You are extracting structured elements from an IETF RFC for a research corpus.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Your output MUST be a single JSON array of element objects, with no surrounding prose, code fences, or commentary. Just `[ {...}, {...}, ... ]`.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Each element MUST have these fields (lowercase, matching the schema):")
	fmt.Fprintln(&b, `  - "kind": one of "normative-requirement", "protocol-element", "wire-format", "state-machine", "registry", "security-consideration", "privacy-consideration", "interoperability-note", "design-rationale", "errata"`)
	fmt.Fprintln(&b, `  - "summary": one to three sentences. Concise; quote or paraphrase the source. No filler.`)
	fmt.Fprintln(&b, `  - "section": the RFC section reference where the element is defined, e.g. "5.1.2". Required when identifiable.`)
	fmt.Fprintln(&b, `  - "rfc2119_level": for normative-requirement only — the RFC 2119 keyword used (MUST, MUST NOT, SHOULD, SHOULD NOT, MAY, REQUIRED, RECOMMENDED, OPTIONAL).`)
	fmt.Fprintln(&b, `  - "topics": optional, ONLY use topic ids that appear in the taxonomy below. If a useful topic is missing, leave the array empty rather than inventing one.`)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Extraction guidance:")
	fmt.Fprintln(&b, "  - Extract the 15-30 most important elements for someone implementing or analyzing this protocol. Don't enumerate every MUST/SHOULD; pick the load-bearing ones.")
	fmt.Fprintln(&b, "  - Always include at least one security-consideration and (if present) privacy-consideration.")
	fmt.Fprintln(&b, "  - Always include the registries this document creates or updates.")
	fmt.Fprintln(&b, "  - For wire-format elements, the summary should reference the field name and its length/encoding.")
	fmt.Fprintln(&b, "  - For state-machine elements, name the states and the trigger.")
	fmt.Fprintln(&b, "  - For design-rationale, prefer items that explain WHY a non-obvious choice was made (often in non-normative prose).")
	fmt.Fprintln(&b, "  - Skip boilerplate (copyright, acknowledgments, IANA boilerplate without registry content).")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "----- TAXONOMY (use only these topic ids) -----")
	fmt.Fprintln(&b, taxonomy)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "----- DOCUMENT METADATA -----")
	fmt.Fprintf(&b, "id: %s\n", r.ID)
	fmt.Fprintf(&b, "title: %s\n", r.Title)
	fmt.Fprintf(&b, "current_status: %s\n", r.CurrentStatus)
	fmt.Fprintf(&b, "stream: %s\n", r.Stream)
	fmt.Fprintf(&b, "area: %s\n", r.Area)
	fmt.Fprintf(&b, "wg: %s\n", r.WG)
	fmt.Fprintf(&b, "date: %s\n", r.Date)
	if r.Abstract != "" {
		fmt.Fprintf(&b, "abstract: |\n  %s\n", strings.ReplaceAll(r.Abstract, "\n", "\n  "))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "----- DOCUMENT FULL TEXT -----")
	fmt.Fprintln(&b, text)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Now emit the JSON array of elements. Output ONLY the JSON. No preamble, no postamble, no fences.")
	return b.String()
}

type elementOut struct {
	Document     string   `yaml:"document" json:"document,omitempty"`
	Kind         string   `yaml:"kind" json:"kind"`
	Summary      string   `yaml:"summary" json:"summary"`
	Section      string   `yaml:"section,omitempty" json:"section,omitempty"`
	RFC2119Level string   `yaml:"rfc2119_level,omitempty" json:"rfc2119_level,omitempty"`
	Topics       []string `yaml:"topics,omitempty" json:"topics,omitempty"`
	ExtractedBy  string   `yaml:"extracted_by,omitempty" json:"extracted_by,omitempty"`
	ExtractedAt  string   `yaml:"extracted_at,omitempty" json:"extracted_at,omitempty"`
}

// runClaude shells out to `claude -p` with retry-with-backoff.
//
// `claude -p` with --output-format json returns its model output on
// stdout. On rate-limit/quota failures it exits non-zero and usually
// writes NOTHING to stderr — the May 2026 incident saw 2,700+ failures
// with empty stderr until we noticed the pattern. Capture both streams
// and include them in the error message so diagnosis isn't a 17-hour
// reverse-engineering exercise next time.
//
// Backoff: claudeMaxAttempts attempts, with delays 10s / 40s / 90s
// (quadratic) between them. Total worst-case wait per extraction:
// ~2.5 minutes of backoff + 3 × 6-min claude timeouts. Move on to
// the next RFC if all attempts fail.
// errClaudeAuth is returned when claude says "Not logged in". The
// extract-all guard treats this as a hard failure and aborts the
// sweep immediately — retrying is useless until the user runs
// `claude /login` in a GUI session.
var errClaudeAuth = fmt.Errorf("claude is not logged in (run `claude /login` in a GUI session)")

func runClaude(ctx context.Context, prompt string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= claudeMaxAttempts; attempt++ {
		out, err := runClaudeOnce(ctx, prompt)
		if err == nil {
			return out, nil
		}
		// Don't retry on auth failures — claude can't log itself in,
		// and burning attempts here just delays the inevitable.
		if strings.Contains(err.Error(), "Not logged in") {
			return "", errClaudeAuth
		}
		lastErr = err
		if attempt < claudeMaxAttempts {
			backoff := time.Duration(attempt*attempt) * 10 * time.Second
			log.Printf("claude attempt %d/%d failed: %v; sleeping %s before retry", attempt, claudeMaxAttempts, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "", fmt.Errorf("claude failed after %d attempts: %w", claudeMaxAttempts, lastErr)
}

func runClaudeOnce(ctx context.Context, prompt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "-p", "--output-format", "json", "--model", claudeModel, prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Capture both streams. claude -p often writes nothing to
		// stderr on rate-limit failures; stdout may contain a JSON
		// error envelope or also be empty.
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
	return stdout.String(), nil
}

// parseElements pulls the JSON array out of the claude output. The
// --output-format json wraps the model reply in a {result: "..."}
// envelope; we strip it and then look for the first [...] array in
// the result text (defensive: model sometimes adds a one-line preamble).
func parseElements(raw string) ([]elementOut, error) {
	var env struct {
		Result string `json:"result"`
	}
	body := raw
	if err := json.Unmarshal([]byte(raw), &env); err == nil && env.Result != "" {
		body = env.Result
	}
	body = stripCodeFence(body)
	start := strings.Index(body, "[")
	end := strings.LastIndex(body, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array in model output: %s", trim(body, 200))
	}
	arr := body[start : end+1]
	var out []elementOut
	if err := json.Unmarshal([]byte(arr), &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w (saw: %s)", err, trim(arr, 200))
	}
	return out, nil
}

var fenceRE = regexp.MustCompile("(?s)```[a-z]*\\n(.*?)```")

func stripCodeFence(s string) string {
	if m := fenceRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── slug for filename ──

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyElement(e elementOut) string {
	base := strings.ToLower(e.Summary)
	base = slugRE.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	parts := strings.Split(base, "-")
	if len(parts) > 8 {
		parts = parts[:8]
	}
	slug := strings.Join(parts, "-")
	if e.Kind != "" {
		slug = e.Kind + "-" + slug
	}
	return slug
}

// ── helpers ──

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

func requireTool(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required tool not found: %s", name)
	}
	return nil
}

var httpClient = &http.Client{Timeout: httpTimeout}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ietf-corpus-elements/0.1 (+https://github.com/getlantern/ietf-corpus)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
