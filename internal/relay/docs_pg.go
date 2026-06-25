package relay

import (
	"database/sql"
)

// DocsTree returns all groups (ordered by position) each with their articles
// (also ordered by position). Bodies are NOT loaded — Article fetches a single
// doc's body when needed.
func (p *PGStore) Tree() ([]DocGroup, error) {
	gr, err := p.db.Query(
		`SELECT id, slug, title, position, updated_at FROM doc_groups ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	groups := []DocGroup{}
	gByID := map[int]int{} // group_id -> slice index
	for gr.Next() {
		var g DocGroup
		gr.Scan(&g.ID, &g.Slug, &g.Title, &g.Position, &g.UpdatedAt)
		gByID[g.ID] = len(groups)
		groups = append(groups, g)
	}
	gr.Close()
	if err := gr.Err(); err != nil {
		return nil, err
	}

	ar, err := p.db.Query(
		`SELECT id, COALESCE(group_id,0), slug, title, position, COALESCE(author_namespace,''), created_at, updated_at
		   FROM docs ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer ar.Close()
	for ar.Next() {
		var a DocArticle
		ar.Scan(&a.ID, &a.GroupID, &a.Slug, &a.Title, &a.Position, &a.AuthorNamespace, &a.CreatedAt, &a.UpdatedAt)
		if idx, ok := gByID[a.GroupID]; ok {
			a.GroupSlug = groups[idx].Slug
			groups[idx].Articles = append(groups[idx].Articles, a)
		}
	}
	return groups, ar.Err()
}

// Article returns one article (with body) by slug, or nil if not found.
func (p *PGStore) Article(slug string) (*DocArticle, error) {
	var a DocArticle
	var groupID sql.NullInt32
	var author sql.NullString
	err := p.db.QueryRow(
		`SELECT d.id, d.group_id, d.slug, d.title, d.body, d.position,
		        d.author_namespace, d.created_at, d.updated_at, COALESCE(g.slug,'')
		   FROM docs d LEFT JOIN doc_groups g ON g.id = d.group_id
		  WHERE d.slug = $1`, slug,
	).Scan(&a.ID, &groupID, &a.Slug, &a.Title, &a.Body, &a.Position,
		&author, &a.CreatedAt, &a.UpdatedAt, &a.GroupSlug)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		a.GroupID = int(groupID.Int32)
	}
	if author.Valid {
		a.AuthorNamespace = author.String
	}
	return &a, nil
}

// UpsertGroup creates or updates a group by slug. Returns the persisted row.
func (p *PGStore) UpsertGroup(g DocGroup) (DocGroup, error) {
	err := p.db.QueryRow(
		`INSERT INTO doc_groups (slug, title, position) VALUES ($1,$2,$3)
		   ON CONFLICT (slug) DO UPDATE
		     SET title = EXCLUDED.title,
		         position = EXCLUDED.position,
		         updated_at = now()
		 RETURNING id, slug, title, position, updated_at`,
		g.Slug, g.Title, g.Position,
	).Scan(&g.ID, &g.Slug, &g.Title, &g.Position, &g.UpdatedAt)
	return g, err
}

// DeleteGroup removes a group. Refuses (ErrDocsGroupHasArticles) if any
// articles still point at it — the caller must move them first.
func (p *PGStore) DeleteGroup(id int) error {
	var n int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM docs WHERE group_id = $1`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDocsGroupHasArticles
	}
	_, err := p.db.Exec(`DELETE FROM doc_groups WHERE id = $1`, id)
	return err
}

// UpsertArticle creates or updates an article by slug. group_id is resolved
// from a.GroupSlug if a.GroupID is zero. Author is recorded on create only.
func (p *PGStore) UpsertArticle(a DocArticle, author string) (DocArticle, error) {
	gid := a.GroupID
	if gid == 0 && a.GroupSlug != "" {
		if err := p.db.QueryRow(`SELECT id FROM doc_groups WHERE slug = $1`, a.GroupSlug).Scan(&gid); err != nil {
			if err == sql.ErrNoRows {
				return DocArticle{}, sql.ErrNoRows
			}
			return DocArticle{}, err
		}
	}
	var groupID any
	if gid != 0 {
		groupID = gid
	}
	err := p.db.QueryRow(
		`INSERT INTO docs (group_id, slug, title, body, position, author_namespace)
		   VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
		   ON CONFLICT (slug) DO UPDATE
		     SET group_id   = COALESCE(EXCLUDED.group_id, docs.group_id),
		         title      = EXCLUDED.title,
		         body       = EXCLUDED.body,
		         position   = EXCLUDED.position,
		         updated_at = now()
		 RETURNING id, COALESCE(group_id,0), slug, title, position,
		           COALESCE(author_namespace,''), created_at, updated_at`,
		groupID, a.Slug, a.Title, a.Body, a.Position, author,
	).Scan(&a.ID, &a.GroupID, &a.Slug, &a.Title, &a.Position,
		&a.AuthorNamespace, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// DeleteArticle removes an article by slug.
func (p *PGStore) DeleteArticle(slug string) error {
	_, err := p.db.Exec(`DELETE FROM docs WHERE slug = $1`, slug)
	return err
}
