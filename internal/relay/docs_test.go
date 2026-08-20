package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memDocs is an in-memory DocsStore used only for tests.
type memDocs struct {
	mu     sync.Mutex
	groups map[int]DocGroup
	arts   map[string]DocArticle // by slug
	nextG  int
	nextA  int
}

func newMemDocs() *memDocs {
	return &memDocs{groups: map[int]DocGroup{}, arts: map[string]DocArticle{}}
}

func (m *memDocs) Tree() ([]DocGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []DocGroup{}
	gByID := map[int]int{}
	for _, g := range m.groups {
		gByID[g.ID] = len(out)
		out = append(out, g)
	}
	for _, a := range m.arts {
		if idx, ok := gByID[a.GroupID]; ok {
			a.GroupSlug = out[idx].Slug
			out[idx].Articles = append(out[idx].Articles, a)
		}
	}
	return out, nil
}

func (m *memDocs) Article(slug string) (*DocArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.arts[slug]
	if !ok {
		return nil, nil
	}
	if g, ok := m.groups[a.GroupID]; ok {
		a.GroupSlug = g.Slug
	}
	return &a, nil
}

func (m *memDocs) UpsertGroup(g DocGroup) (DocGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.groups {
		if existing.Slug == g.Slug {
			g.ID = existing.ID
			break
		}
	}
	if g.ID == 0 {
		m.nextG++
		g.ID = m.nextG
	}
	g.UpdatedAt = time.Now()
	m.groups[g.ID] = g
	return g, nil
}

func (m *memDocs) DeleteGroup(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.arts {
		if a.GroupID == id {
			return ErrDocsGroupHasArticles
		}
	}
	delete(m.groups, id)
	return nil
}

func (m *memDocs) UpsertArticle(a DocArticle, author string) (DocArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.GroupID == 0 && a.GroupSlug != "" {
		for _, g := range m.groups {
			if g.Slug == a.GroupSlug {
				a.GroupID = g.ID
			}
		}
	}
	existing, exists := m.arts[a.Slug]
	if !exists {
		m.nextA++
		a.ID = m.nextA
		a.AuthorNamespace = author
		a.CreatedAt = time.Now()
	} else {
		a.ID = existing.ID
		a.AuthorNamespace = existing.AuthorNamespace
		a.CreatedAt = existing.CreatedAt
	}
	a.UpdatedAt = time.Now()
	m.arts[a.Slug] = a
	return a, nil
}

func (m *memDocs) DeleteArticle(slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.arts, slug)
	return nil
}

// envTokens is a tiny TokenStore where token => namespace.
type envTokens map[string]string

func (e envTokens) Resolve(t string) (string, bool) { ns, ok := e[t]; return ns, ok }

func newTestRelay(t *testing.T) (*Relay, *memDocs) {
	t.Helper()
	r := New(envTokens{"tok-alice": "alice"})
	d := newMemDocs()
	r.SetDocs(d)
	r.SetAdminSecret("test-secret")
	// Admin store stub: only used to gate adminOK on r.admin != nil.
	r.admin = &noopAdmin{}
	return r, d
}

type noopAdmin struct{}

func (n *noopAdmin) ResolveUser(string) (string, error) { return "", nil }
func (n *noopAdmin) ResolveIdentity(string, string, string, string, string, string) (string, string, error) {
	return "", "", nil
}
func (n *noopAdmin) CreateInvite(string) (Invite, string, error)    { return Invite{}, "", nil }
func (n *noopAdmin) ListInvites() ([]Invite, error)                 { return nil, nil }
func (n *noopAdmin) RevokeInvite(int) (bool, error)                 { return false, nil }
func (n *noopAdmin) UpsertDevice(string, string, string)            {}
func (n *noopAdmin) IssueToken(string, string, int) (string, error) { return "", nil }
func (n *noopAdmin) ListTokens(string) ([]map[string]any, error)    { return nil, nil }
func (n *noopAdmin) RevokeToken(string, int) error                  { return nil }
func (n *noopAdmin) ListDevices(string) ([]map[string]any, error)   { return nil, nil }
func (n *noopAdmin) ListLarkApproval(string) ([]DeviceLarkApproval, error) {
	return nil, nil
}
func (n *noopAdmin) UpsertLarkApproval(cfg DeviceLarkApproval) (DeviceLarkApproval, error) {
	return cfg, nil
}
func (n *noopAdmin) ListUsers() ([]string, error)                { return nil, nil }
func (n *noopAdmin) RemoveDevice(string, string) error           { return nil }
func (n *noopAdmin) ListACL(string) ([]map[string]any, error)    { return nil, nil }
func (n *noopAdmin) AddACL(string, string, string, string) error { return nil }
func (n *noopAdmin) RevokeACL(string, int) error                 { return nil }
func (n *noopAdmin) ListAudit(string) ([]map[string]any, error)  { return nil, nil }

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestDocsLifecycle(t *testing.T) {
	r, _ := newTestRelay(t)
	h := r.Handler()

	// 1) public tree is empty
	rr := do(t, h, "GET", "/docs/tree.json", "")
	if rr.Code != 200 {
		t.Fatalf("tree status %d body %s", rr.Code, rr.Body.String())
	}
	var tree struct{ Groups []DocGroup }
	json.NewDecoder(rr.Body).Decode(&tree)
	if len(tree.Groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(tree.Groups))
	}

	// 2) namespace-token creates a group
	rr = do(t, h, "POST", "/docs/groups?token=tok-alice",
		`{"slug":"quickstart","title":"快速开始","position":10}`)
	if rr.Code != 200 {
		t.Fatalf("group create %d %s", rr.Code, rr.Body.String())
	}

	// 3) and an article
	rr = do(t, h, "POST", "/docs/articles?token=tok-alice",
		`{"slug":"hello","title":"Hello","body":"# hi","group_slug":"quickstart","position":1}`)
	if rr.Code != 200 {
		t.Fatalf("article create %d %s", rr.Code, rr.Body.String())
	}

	// 4) read it back
	rr = do(t, h, "GET", "/docs/hello.json", "")
	if rr.Code != 200 {
		t.Fatalf("article get %d %s", rr.Code, rr.Body.String())
	}
	var got DocArticle
	json.NewDecoder(rr.Body).Decode(&got)
	if got.Body != "# hi" || got.GroupSlug != "quickstart" || got.AuthorNamespace != "alice" {
		t.Fatalf("article roundtrip wrong: %+v", got)
	}

	// 5) tree now shows it
	rr = do(t, h, "GET", "/docs/tree.json", "")
	json.NewDecoder(rr.Body).Decode(&tree)
	if len(tree.Groups) != 1 || len(tree.Groups[0].Articles) != 1 {
		t.Fatalf("tree wrong: %+v", tree.Groups)
	}

	// 6) unauthenticated write is rejected
	rr = do(t, h, "POST", "/docs/articles",
		`{"slug":"x","title":"x","body":"x","group_slug":"quickstart"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}

	// 7) deleting a group with articles fails 409
	rr = do(t, h, "POST", "/docs/groups/delete?token=tok-alice",
		`{"slug":"quickstart"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", rr.Code, rr.Body.String())
	}

	// 8) delete article then group succeeds
	rr = do(t, h, "POST", "/docs/articles/delete?token=tok-alice", `{"slug":"hello"}`)
	if rr.Code != 200 {
		t.Fatalf("article delete %d %s", rr.Code, rr.Body.String())
	}
	rr = do(t, h, "POST", "/docs/groups/delete?token=tok-alice", `{"slug":"quickstart"}`)
	if rr.Code != 200 {
		t.Fatalf("group delete %d %s", rr.Code, rr.Body.String())
	}
}

func TestDocsAdminMirror(t *testing.T) {
	r, _ := newTestRelay(t)
	h := r.Handler()

	// no secret → 403
	rr := do(t, h, "POST", "/admin/docs/groups",
		`{"slug":"g","title":"G","position":1}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no-secret expected 403 got %d", rr.Code)
	}

	// with secret → ok, and author propagates
	req := httptest.NewRequest("POST", "/admin/docs/groups",
		bytes.NewReader([]byte(`{"slug":"g","title":"G","position":1,"author":"bob"}`)))
	req.Header.Set("X-Admin-Secret", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("admin upsert %d %s", rr.Code, rr.Body.String())
	}

	// article via admin
	req = httptest.NewRequest("POST", "/admin/docs/articles",
		bytes.NewReader([]byte(`{"slug":"a","title":"A","body":"B","group_slug":"g","author":"bob"}`)))
	req.Header.Set("X-Admin-Secret", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("admin article %d %s", rr.Code, rr.Body.String())
	}

	rr = do(t, h, "GET", "/docs/a.json", "")
	var got DocArticle
	json.NewDecoder(rr.Body).Decode(&got)
	if got.AuthorNamespace != "bob" {
		t.Fatalf("author not propagated: %+v", got)
	}
}
