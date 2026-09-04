package portal

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The SPA is three plain files with no build step, so nothing tells you when a
// rename in index.html leaves app.js reaching for an element that is gone.
// $('#x') on a missing element returns null, and the next property access
// throws — at module scope that kills the whole script, and the page renders as
// a logged-out empty state rather than as an error. That failure looks like a
// backend problem and costs an afternoon.
//
// This is the cheap guard: every id app.js looks up must either exist in
// index.html or be listed here as one the script creates itself.
var idsCreatedByScript = map[string]bool{
	"idOK": true, // renderIdentityChanged writes this button into #dAsks
	"obGo": true, // renderDevices writes this button into #onboard
}

func TestScriptOnlyLooksUpElementsThatExist(t *testing.T) {
	js := readWeb(t, "web/app.js")
	html := readWeb(t, "web/index.html")

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(html, -1) {
		declared[m[1]] = true
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\('#([A-Za-z0-9_-]+)'\)`).FindAllStringSubmatch(js, -1) {
		id := m[1]
		if seen[id] || declared[id] || idsCreatedByScript[id] {
			continue
		}
		seen[id] = true
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js looks up ids that index.html does not declare: %s\n"+
			"Either the element was renamed, or the script creates it — if it creates it, "+
			"add it to idsCreatedByScript with a note saying where.", strings.Join(missing, ", "))
	}
}

// The asset URLs in index.html carry the fingerprint that assets.go substitutes
// at start. Losing the placeholder would not fail any build; it would quietly
// re-enable the stale-CSS-with-new-HTML pairing that the fingerprint exists to
// prevent.
func TestIndexAsksForVersionedAssets(t *testing.T) {
	html := readWeb(t, "web/index.html")
	for _, want := range []string{`href="/assets/app.css?v=__V__"`, `src="/assets/app.js?v=__V__"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html no longer requests %s", want)
		}
	}
	if strings.Contains(string(indexPage), "__V__") {
		t.Error("the served page still contains the __V__ placeholder; assets.go did not substitute it")
	}
	if len(assetVersion) != 10 {
		t.Errorf("assetVersion = %q, want 10 hex characters", assetVersion)
	}
}

// auth.js runs on three separate pages and each carries only part of what it
// touches, so the rule is different from the SPA's: an id must exist on at
// least one page, and the ids it reaches for unconditionally must exist on all
// three. #lang is the unconditional one — losing it there kills the script
// before the language switch is wired, on the very first page of the product.
func TestAuthScriptMatchesItsPages(t *testing.T) {
	js := readWeb(t, "web/auth.js")
	pageNames := []string{"web/login.html", "web/pending.html", "web/enroll.html"}

	id := regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	anywhere := map[string]bool{}
	for _, p := range pageNames {
		html := readWeb(t, p)
		for _, m := range id.FindAllStringSubmatch(html, -1) {
			anywhere[m[1]] = true
		}
		for _, need := range []string{"lang"} {
			if !strings.Contains(html, `id="`+need+`"`) {
				t.Errorf("%s has no #%s; auth.js looks it up on every page", p, need)
			}
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\('#([A-Za-z0-9_-]+)'\)`).FindAllStringSubmatch(js, -1) {
		if seen[m[1]] || anywhere[m[1]] {
			continue
		}
		seen[m[1]] = true
		missing = append(missing, m[1])
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("auth.js looks up ids no auth page declares: %s", strings.Join(missing, ", "))
	}
}

func readWeb(t *testing.T, name string) string {
	t.Helper()
	b, err := assets.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
