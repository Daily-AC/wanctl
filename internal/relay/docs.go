package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// DocGroup is one sidebar section.
type DocGroup struct {
	ID        int           `json:"id"`
	Slug      string        `json:"slug"`
	Title     string        `json:"title"`
	Position  int           `json:"position"`
	UpdatedAt time.Time     `json:"updated_at"`
	Articles  []DocArticle  `json:"articles,omitempty"`
}

// DocArticle is one document. Body is markdown.
type DocArticle struct {
	ID              int       `json:"id"`
	GroupID         int       `json:"group_id"`
	GroupSlug       string    `json:"group_slug,omitempty"`
	Slug            string    `json:"slug"`
	Title           string    `json:"title"`
	Body            string    `json:"body,omitempty"`
	Position        int       `json:"position"`
	AuthorNamespace string    `json:"author_namespace,omitempty"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// DocsStore is the persistence surface for the docs site. PGStore implements it
// against Postgres; tests substitute an in-memory fake.
type DocsStore interface {
	Tree() ([]DocGroup, error)
	Article(slug string) (*DocArticle, error)
	UpsertGroup(g DocGroup) (DocGroup, error)
	DeleteGroup(id int) error
	UpsertArticle(a DocArticle, author string) (DocArticle, error)
	DeleteArticle(slug string) error
}

// SetDocs installs the docs store.
func (r *Relay) SetDocs(d DocsStore) { r.docs = d }

// registerDocs wires public-read and namespace-token-write doc endpoints.
// (Admin-secret-gated mirrors live in admin.go's registerAdmin.)
func (r *Relay) registerDocs(mux *http.ServeMux) {
	mux.HandleFunc("/docs/tree.json", r.handleDocsTree)
	mux.HandleFunc("/docs/", r.handleDocsArticle) // GET /docs/<slug>.json
	mux.HandleFunc("/docs/groups", r.handleDocsGroupsNS)
	mux.HandleFunc("/docs/groups/delete", r.handleDocsGroupDeleteNS)
	mux.HandleFunc("/docs/articles", r.handleDocsArticlesNS)
	mux.HandleFunc("/docs/articles/delete", r.handleDocsArticleDeleteNS)
}

// --- public read ---

func (r *Relay) handleDocsTree(w http.ResponseWriter, req *http.Request) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	groups, err := r.docs.Tree()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"groups": groups})
}

func (r *Relay) handleDocsArticle(w http.ResponseWriter, req *http.Request) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	// /docs/<slug>.json
	rest := strings.TrimPrefix(req.URL.Path, "/docs/")
	if rest == "" || rest == "tree.json" {
		http.NotFound(w, req)
		return
	}
	slug := strings.TrimSuffix(rest, ".json")
	if slug == rest {
		// no .json suffix
		http.NotFound(w, req)
		return
	}
	a, err := r.docs.Article(slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if a == nil {
		http.NotFound(w, req)
		return
	}
	writeJSON(w, a)
}

// --- namespace-token write (anyone with a valid relay token can edit;
// audit logs the namespace responsible). Internal-team trust model. ---

func (r *Relay) handleDocsGroupsNS(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.docsUpsertGroup(w, req, ns)
}

func (r *Relay) handleDocsGroupDeleteNS(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.docsDeleteGroup(w, req)
}

func (r *Relay) handleDocsArticlesNS(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.docsUpsertArticle(w, req, ns)
}

func (r *Relay) handleDocsArticleDeleteNS(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.docsDeleteArticle(w, req)
}

// --- shared upsert/delete helpers (used by both NS and admin-secret paths) ---

func (r *Relay) docsUpsertGroup(w http.ResponseWriter, req *http.Request, author string) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	var body DocGroup
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Slug == "" || body.Title == "" {
		http.Error(w, "slug and title required", http.StatusBadRequest)
		return
	}
	out, err := r.docs.UpsertGroup(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.audit != nil {
		r.audit.Audit(author, "", "docs:group:upsert:"+out.Slug)
	}
	writeJSON(w, out)
}

func (r *Relay) docsDeleteGroup(w http.ResponseWriter, req *http.Request) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	}
	json.NewDecoder(req.Body).Decode(&body)
	if body.ID == 0 && body.Slug == "" {
		http.Error(w, "id or slug required", http.StatusBadRequest)
		return
	}
	// If slug is given, resolve to id by scanning the tree (cheap; small set).
	if body.ID == 0 {
		groups, err := r.docs.Tree()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, g := range groups {
			if g.Slug == body.Slug {
				body.ID = g.ID
				break
			}
		}
	}
	if body.ID == 0 {
		http.NotFound(w, req)
		return
	}
	if err := r.docs.DeleteGroup(body.ID); err != nil {
		if errors.Is(err, ErrDocsGroupHasArticles) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) docsUpsertArticle(w http.ResponseWriter, req *http.Request, author string) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	var body DocArticle
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Slug == "" || body.Title == "" {
		http.Error(w, "slug and title required", http.StatusBadRequest)
		return
	}
	out, err := r.docs.UpsertArticle(body, author)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.audit != nil {
		r.audit.Audit(author, "", "docs:article:upsert:"+out.Slug)
	}
	out.Body = "" // omit body in upsert response (caller already has it)
	writeJSON(w, out)
}

func (r *Relay) docsDeleteArticle(w http.ResponseWriter, req *http.Request) {
	if r.docs == nil {
		http.Error(w, "docs not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Slug string `json:"slug"`
	}
	json.NewDecoder(req.Body).Decode(&body)
	if body.Slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	if err := r.docs.DeleteArticle(body.Slug); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ErrDocsGroupHasArticles is returned by DeleteGroup when articles still
// reference the group; surfaced as 409 Conflict to the caller.
var ErrDocsGroupHasArticles = errors.New("group has articles; reassign or delete them first")
