package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/prizm"
	"github.com/sxty9/prizm/subprizm"
)

// action is the validated, resolved outcome of one LLM decision: what to keep from this page,
// which enumerated assets to download, and which enumerated links to follow.
type action struct {
	Keep      bool
	Title     string
	Kategorie string
	Downloads []resolvedDownload
	Follows   []string // absolute follow URLs (resolved from candidate indices)
	Done      bool
}

type resolvedDownload struct {
	URL       string
	Title     string
	Kategorie string
}

// rawAction is the strict JSON shape the model must emit. It references candidates by the
// index scrapr enumerated — scrapr resolves the URL from its OWN list, so the model can never
// introduce an off-domain or invented URL (the core guardrail).
type rawAction struct {
	Extract struct {
		Keep      bool   `json:"keep"`
		Title     string `json:"title"`
		Kategorie string `json:"kategorie"`
		Summary   string `json:"summary"`
	} `json:"extract"`
	Follow []struct {
		Index  int    `json:"index"`
		Reason string `json:"reason"`
	} `json:"follow"`
	Download []struct {
		Index     int    `json:"index"`
		Title     string `json:"title"`
		Kategorie string `json:"kategorie"`
		Reason    string `json:"reason"`
	} `json:"download"`
	Done bool `json:"done"`
}

// decideNext asks aigentic's choose router what to do with this page and returns the validated,
// resolved action. An engine-unavailability error propagates so the coordinator can surface 503.
func (p *processor) decideNext(ctx context.Context, env prizm.Env, goal string, page *pageData) (action, error) {
	req := aigentic.Request{
		Prompt:       buildDecidePrompt(goal, page),
		OutputFormat: "json",
		Model:        p.cfg.DecideModel,
		MaxTokens:    p.cfg.MaxDecideTokens,
		Choose:       &aigentic.ChooseOptions{Force: p.cfg.ForceEngine},
	}
	res, err := subprizm.SpawnTyped[aigentic.Request, aigentic.Result](ctx, env.Spawn, env.Header, aigentic.KindChoose, req)
	if err != nil {
		return action{}, err
	}
	return parseAction(res.Output, page)
}

// buildDecidePrompt renders the goal, page context and enumerated candidates into an
// instruction that constrains the model to the strict action JSON.
func buildDecidePrompt(goal string, page *pageData) string {
	var b strings.Builder
	b.WriteString("You are the decision step of an agentic web scraper. Decide what to do with the current page to satisfy the goal.\n\n")
	b.WriteString("GOAL: " + strings.TrimSpace(goal) + "\n\n")
	b.WriteString("CURRENT PAGE\nurl: " + page.URL + "\ntitle: " + page.Title + "\n\ntext (truncated):\n")
	b.WriteString(truncate(page.Markdown, 4000))
	b.WriteString("\n\nCANDIDATE LINKS (follow by index; only these may be followed):\n")
	if len(page.Links) == 0 {
		b.WriteString("(none)\n")
	}
	for i, c := range page.Links {
		b.WriteString(fmt.Sprintf("[%d] %s — %s\n", i, truncate(c.Anchor, 80), c.URL))
	}
	b.WriteString("\nCANDIDATE DOWNLOADS (files/media; download by index; only these may be downloaded):\n")
	if len(page.Downloads) == 0 {
		b.WriteString("(none)\n")
	}
	for i, c := range page.Downloads {
		b.WriteString(fmt.Sprintf("[%d] %s — %s\n", i, truncate(c.Anchor, 80), c.URL))
	}
	b.WriteString("\nRespond with ONLY a JSON object of this exact shape (no prose, no code fence):\n")
	b.WriteString(`{"extract":{"keep":<bool>,"title":"<string>","kategorie":"<one of: ` + strings.Join(KATEGORIEN, ", ") + `>","summary":"<one line>"},`)
	b.WriteString(`"follow":[{"index":<int>,"reason":"<why>"}],`)
	b.WriteString(`"download":[{"index":<int>,"title":"<string>","kategorie":"<one of the above>","reason":"<why>"}],`)
	b.WriteString(`"done":<bool>}` + "\n")
	b.WriteString("\nRules: keep=true if this page's text is relevant to the goal. Only reference indices that appear above. ")
	b.WriteString("Pick kategorie from the listed set. Follow only links that advance the goal; set done=true when nothing more is worth visiting from here.")
	return b.String()
}

// parseAction extracts the JSON object from the model output, validates it, and resolves each
// index against the page's candidate lists. Out-of-range indices and bad kategorien are dropped
// or defaulted — the model can only choose from what scrapr found.
func parseAction(output string, page *pageData) (action, error) {
	// Degrade gracefully when the model returns no usable JSON (common with small local models
	// on large pages): keep this page's content, follow/download nothing, and stop here. The
	// page is still captured; only navigation from it is skipped.
	keepOnly := action{Keep: true, Kategorie: defaultKategorie, Done: true}
	js := extractJSONObject(output)
	if js == "" {
		return keepOnly, nil
	}
	var ra rawAction
	if err := json.Unmarshal([]byte(js), &ra); err != nil {
		return keepOnly, nil
	}
	act := action{
		Keep:      ra.Extract.Keep,
		Title:     clean(ra.Extract.Title),
		Kategorie: validKategorie(ra.Extract.Kategorie),
		Done:      ra.Done,
	}
	seenFollow := map[int]bool{}
	for _, f := range ra.Follow {
		if f.Index < 0 || f.Index >= len(page.Links) || seenFollow[f.Index] {
			continue
		}
		seenFollow[f.Index] = true
		act.Follows = append(act.Follows, page.Links[f.Index].URL)
	}
	seenDown := map[int]bool{}
	for _, d := range ra.Download {
		if d.Index < 0 || d.Index >= len(page.Downloads) || seenDown[d.Index] {
			continue
		}
		seenDown[d.Index] = true
		act.Downloads = append(act.Downloads, resolvedDownload{
			URL:       page.Downloads[d.Index].URL,
			Title:     firstNonEmpty(clean(d.Title), page.Downloads[d.Index].Anchor),
			Kategorie: validKategorie(d.Kategorie),
		})
	}
	return act, nil
}

// extractJSONObject returns the first balanced {...} object in s (tolerating a code fence or
// stray prose around it), or "".
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// inside string literal: ignore braces
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
