package relay

import (
	"regexp"
	"strings"
	"testing"

	"agent-relay/internal/web"
)

// readV1LinkAsset loads an embedded static asset (readCfgAsset pattern, see
// console_v2_config_panel_test.go:13 — web.StaticFiles.ReadFile).
func readV1LinkAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := web.StaticFiles.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// hdrRightBlock returns the inner HTML of the v2 header's <div class="hdr-right">.
// hdr-right holds no nested <div>, so the first </div> after it closes it.
func hdrRightBlock(t *testing.T, html string) string {
	t.Helper()
	open := strings.Index(html, `<div class="hdr-right">`)
	if open < 0 {
		t.Fatal("v2 index: no <div class=\"hdr-right\"> in header")
	}
	rest := html[open:]
	end := strings.Index(rest, "</div>")
	if end < 0 {
		t.Fatal("v2 index: hdr-right div is not closed")
	}
	return rest[:end]
}

var v1LinkAnchorRe = regexp.MustCompile(`<a\b[^>]*\bid="v1-link"[^>]*>`)

// TestV2HeaderV1LinkPresent — AC1: the v2 header carries a keyboard-reachable back-link
// to the v1 console: an <a> with id="v1-link", href="/", a class containing
// back-chip, a non-empty aria-label, and NO tabindex, sitting inside hdr-right.
func TestV2HeaderV1LinkPresent(t *testing.T) {
	html := readV1LinkAsset(t, "static/v2/index.html")
	block := hdrRightBlock(t, html)

	tag := v1LinkAnchorRe.FindString(block)
	if tag == "" {
		t.Fatal("hdr-right has no <a id=\"v1-link\"> back-link")
	}
	if !strings.Contains(tag, `href="/"`) {
		t.Errorf("v1-link href is not \"/\": %q", tag)
	}
	cls := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(tag)
	if cls == nil || !strings.Contains(cls[1], "back-chip") {
		t.Errorf("v1-link class does not contain back-chip: %q", tag)
	}
	al := regexp.MustCompile(`aria-label="([^"]*)"`).FindStringSubmatch(tag)
	if al == nil || strings.TrimSpace(al[1]) == "" {
		t.Errorf("v1-link has no non-empty aria-label: %q", tag)
	}
	if strings.Contains(tag, "tabindex") {
		t.Errorf("v1-link carries a tabindex (must be natively focusable): %q", tag)
	}
}

// TestV1AssetsUntouchedByV1Link — AC2: the id v1-link appears in NO v1 asset, and v1's
// existing id="v2-link" anchor (href="/v2/") is still present unchanged.
func TestV1AssetsUntouchedByV1Link(t *testing.T) {
	for _, name := range []string{"static/index.html", "static/style.css", "static/js/main.js"} {
		a := readV1LinkAsset(t, name)
		if strings.Contains(a, "v1-link") {
			t.Errorf("v1 asset %s leaked the v2 id v1-link", name)
		}
	}
	v1 := readV1LinkAsset(t, "static/index.html")
	v2Anchor := regexp.MustCompile(`<a\b[^>]*\bid="v2-link"[^>]*>`).FindString(v1)
	if v2Anchor == "" {
		t.Fatal("v1 index no longer has its id=\"v2-link\" anchor")
	}
	if !strings.Contains(v2Anchor, `href="/v2/"`) {
		t.Errorf("v1 v2-link anchor href changed (want /v2/): %q", v2Anchor)
	}
}

// TestV2HeaderLayoutPreservedSmoke — AC3: the live status span id="liveStatus" is
// still inside hdr-right, and it sits AFTER the v1 back-link (layout preserved).
func TestV2HeaderLayoutPreservedSmoke(t *testing.T) {
	html := readV1LinkAsset(t, "static/v2/index.html")
	block := hdrRightBlock(t, html)

	linkIdx := strings.Index(block, `id="v1-link"`)
	liveIdx := strings.Index(block, `id="liveStatus"`)
	if linkIdx < 0 {
		t.Fatal("hdr-right has no v1-link")
	}
	if liveIdx < 0 {
		t.Fatal("hdr-right no longer has the liveStatus span")
	}
	if linkIdx >= liveIdx {
		t.Errorf("v1-link must sit before liveStatus (link at %d, live at %d)", linkIdx, liveIdx)
	}
}
