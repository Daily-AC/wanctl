package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckAdminJSONKeys(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{``, true},
		{`{}`, true},
		{`{"namespace":"a","label":"标签","days":3}`, true},
		{`{"namespace":"a","list":[{"x":1},{"x":2}],"obj":{"x":"y"}}`, true},
		{`["namespace","namespace"]`, true},
		{`{"namespace":"a","nameſpace":"b"}`, false}, // U+017F folds to s
		{`{"nameſpace":"b"}`, false},
		{"{\"\u212a\":1}", false}, // U+212A KELVIN SIGN folds to k
		{`{"namespace":"a","Namespace":"b"}`, false},
		{`{"namespace":"a","namespace":"b"}`, false},
		{`{"outer":{"id":1,"ID":2}}`, false},
		{`{"namespace":`, false},
	} {
		err := checkAdminJSONKeys([]byte(tc.body))
		if (err == nil) != tc.ok {
			t.Errorf("checkAdminJSONKeys(%s) = %v, want ok=%v", tc.body, err, tc.ok)
		}
	}
}

type issueRecorder struct {
	AdminStore
	issued []string
}

func (s *issueRecorder) IssueToken(ns, label string, days int) (string, error) {
	s.issued = append(s.issued, ns)
	return "synthetic-test-token", nil
}

// The portal sets "namespace" next to the fields the user sent. A key that
// encoding/json would fold onto the same field must not reach the handler.
func TestAdminRefusesAFoldedNamespaceKey(t *testing.T) {
	store := &issueRecorder{}
	r := New(EnvTokenStore("test:attacker"))
	r.SetAdmin(store)
	r.SetAdminSecret("synthetic-admin-secret-0123456789abcdef")
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/tokens/issue", strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", "synthetic-admin-secret-0123456789abcdef")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(`{"label":"x","namespace":"attacker","nameſpace":"victim"}`); code != http.StatusBadRequest {
		t.Fatalf("folded duplicate: status %d, want 400", code)
	}
	if len(store.issued) != 0 {
		t.Fatalf("a token was issued for %v", store.issued)
	}
	if code := post(`{"label":"x","days":0,"namespace":"attacker"}`); code != http.StatusOK {
		t.Fatalf("plain body: status %d, want 200", code)
	}
	if len(store.issued) != 1 || store.issued[0] != "attacker" {
		t.Fatalf("issued = %v, want [attacker]", store.issued)
	}
}
