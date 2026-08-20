package relay

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var friendDriverID uint64

type friendRecord struct {
	id                   int
	requester, addressee string
	status               string
	created, accepted    time.Time
}

type friendACLRecord struct {
	id                     int
	owner, device, grantee string
	perms                  string
	revoked                bool
}

type friendState struct {
	users  map[string]bool
	friend *friendRecord
	acl    []friendACLRecord
}

type friendDriver struct{ state *friendState }

func (d friendDriver) Open(string) (driver.Conn, error) { return friendConn{state: d.state}, nil }

type friendConn struct{ state *friendState }

func (c friendConn) Prepare(query string) (driver.Stmt, error) {
	return friendStmt{state: c.state, query: query}, nil
}
func (friendConn) Close() error              { return nil }
func (friendConn) Begin() (driver.Tx, error) { return friendTx{}, nil }

type friendTx struct{}

func (friendTx) Commit() error   { return nil }
func (friendTx) Rollback() error { return nil }

type friendStmt struct {
	state *friendState
	query string
}

func (friendStmt) Close() error  { return nil }
func (friendStmt) NumInput() int { return -1 }

func (s friendStmt) Exec(args []driver.Value) (driver.Result, error) {
	switch {
	case strings.Contains(s.query, "pg_advisory_xact_lock"):
		return driver.RowsAffected(0), nil
	case strings.Contains(s.query, "UPDATE friends SET status = 'accepted'"):
		first, second := args[0].(string), args[1].(string)
		matches := s.state.friend != nil && s.state.friend.status == "pending"
		if strings.Contains(s.query, "WHERE requester_ns") {
			matches = matches && s.state.friend.requester == first && s.state.friend.addressee == second
		} else {
			matches = matches && s.state.friend.addressee == first && s.state.friend.requester == second
		}
		if !matches {
			return driver.RowsAffected(0), nil
		}
		s.state.friend.status = "accepted"
		s.state.friend.accepted = time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
		return driver.RowsAffected(1), nil
	case strings.Contains(s.query, "DELETE FROM friends WHERE addressee_ns"):
		ns, requester := args[0].(string), args[1].(string)
		if s.state.friend == nil || s.state.friend.status != "pending" ||
			s.state.friend.addressee != ns || s.state.friend.requester != requester {
			return driver.RowsAffected(0), nil
		}
		s.state.friend = nil
		return driver.RowsAffected(1), nil
	case strings.Contains(s.query, "DELETE FROM friends"):
		a, b := args[0].(string), args[1].(string)
		if s.state.friend == nil || !samePair(a, b, s.state.friend.requester, s.state.friend.addressee) {
			return driver.RowsAffected(0), nil
		}
		s.state.friend = nil
		return driver.RowsAffected(1), nil
	case strings.Contains(s.query, "UPDATE acl SET revoked_at"):
		a, b := args[0].(string), args[1].(string)
		var affected int64
		for i := range s.state.acl {
			acl := &s.state.acl[i]
			if !acl.revoked && ((acl.owner == a && acl.grantee == b) || (acl.owner == b && acl.grantee == a)) {
				acl.revoked = true
				affected++
			}
		}
		return driver.RowsAffected(affected), nil
	default:
		return nil, fmt.Errorf("unexpected exec: %s", s.query)
	}
}

func (s friendStmt) Query(args []driver.Value) (driver.Rows, error) {
	switch {
	case strings.Contains(s.query, "SELECT EXISTS (SELECT 1 FROM users"):
		namespace := args[0].(string)
		return friendRows([]string{"exists"}, [][]driver.Value{{s.state.users[namespace]}}), nil
	case strings.Contains(s.query, "SELECT EXISTS") && strings.Contains(s.query, "FROM friends"):
		a, b := args[0].(string), args[1].(string)
		exists := s.state.friend != nil && s.state.friend.status == "accepted" &&
			samePair(a, b, s.state.friend.requester, s.state.friend.addressee)
		return friendRows([]string{"exists"}, [][]driver.Value{{exists}}), nil
	case strings.Contains(s.query, "SELECT requester_ns, addressee_ns, status FROM friends"):
		a, b := args[0].(string), args[1].(string)
		if s.state.friend == nil || !samePair(a, b, s.state.friend.requester, s.state.friend.addressee) {
			return friendRows([]string{"requester_ns", "addressee_ns", "status"}, nil), nil
		}
		f := s.state.friend
		return friendRows([]string{"requester_ns", "addressee_ns", "status"}, [][]driver.Value{{f.requester, f.addressee, f.status}}), nil
	case strings.Contains(s.query, "INSERT INTO friends"):
		created := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
		s.state.friend = &friendRecord{id: 1, requester: args[0].(string), addressee: args[1].(string), status: "pending", created: created}
		return friendRows([]string{"status"}, [][]driver.Value{{"pending"}}), nil
	case strings.Contains(s.query, "SELECT CASE WHEN requester_ns"):
		namespace := args[0].(string)
		if s.state.friend == nil || (s.state.friend.requester != namespace && s.state.friend.addressee != namespace) {
			return friendRows([]string{"namespace", "status", "direction", "since"}, nil), nil
		}
		f := s.state.friend
		other, direction, since := f.requester, "incoming", f.created
		if f.requester == namespace {
			other, direction = f.addressee, "outgoing"
		}
		if f.status == "accepted" {
			direction, since = "", f.accepted
		}
		return friendRows([]string{"namespace", "status", "direction", "since"}, [][]driver.Value{{other, f.status, direction, since}}), nil
	case strings.Contains(s.query, "SELECT id FROM friends") && strings.Contains(s.query, "FOR SHARE"):
		a, b := args[0].(string), args[1].(string)
		if s.state.friend == nil || s.state.friend.status != "accepted" ||
			!samePair(a, b, s.state.friend.requester, s.state.friend.addressee) {
			return friendRows([]string{"id"}, nil), nil
		}
		return friendRows([]string{"id"}, [][]driver.Value{{int64(s.state.friend.id)}}), nil
	case strings.Contains(s.query, "INSERT INTO acl"):
		id := len(s.state.acl) + 1
		s.state.acl = append(s.state.acl, friendACLRecord{
			id: id, owner: args[0].(string), device: args[1].(string),
			grantee: args[2].(string), perms: args[3].(string),
		})
		return friendRows([]string{"id"}, [][]driver.Value{{int64(id)}}), nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", s.query)
	}
}

func samePair(a, b, c, d string) bool { return (a == c && b == d) || (a == d && b == c) }

func friendRows(columns []string, values [][]driver.Value) driver.Rows {
	return &adminRows{columns: columns, values: values}
}

func newFriendTestStore(t *testing.T, state *friendState) *PGStore {
	t.Helper()
	name := fmt.Sprintf("wanctl_friend_test_%d", atomic.AddUint64(&friendDriverID, 1))
	sql.Register(name, friendDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return &PGStore{db: db}
}

func TestFriendRequestAcceptFlow(t *testing.T) {
	state := &friendState{users: map[string]bool{"alice": true, "bob": true}}
	store := newFriendTestStore(t, state)
	status, err := store.FriendRequest("alice", "bob", "portal")
	if err != nil || status != "pending" {
		t.Fatalf("request = %q, %v; want pending", status, err)
	}
	for _, tc := range []struct {
		namespace, direction string
	}{{"alice", "outgoing"}, {"bob", "incoming"}} {
		friends, err := store.ListFriends(tc.namespace)
		if err != nil || len(friends) != 1 || friends[0].Direction != tc.direction || friends[0].Status != "pending" {
			t.Fatalf("ListFriends(%q) = %+v, %v; want pending %s", tc.namespace, friends, err, tc.direction)
		}
	}
	if err := store.FriendAccept("bob", "alice"); err != nil {
		t.Fatal(err)
	}
	friends, err := store.ListFriends("alice")
	if err != nil || len(friends) != 1 || friends[0].Status != "accepted" || friends[0].Direction != "" {
		t.Fatalf("friends = %+v, %v", friends, err)
	}
	ok, err := store.IsFriend("alice", "bob")
	if err != nil || !ok {
		t.Fatalf("IsFriend = %v, %v", ok, err)
	}
}

func TestFriendRequestIsIdempotentAndChecksAddressee(t *testing.T) {
	state := &friendState{users: map[string]bool{"alice": true, "bob": true}}
	store := newFriendTestStore(t, state)
	if _, err := store.FriendRequest("alice", "missing", "portal"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("missing addressee error = %v", err)
	}
	if status, err := store.FriendRequest("alice", "bob", "portal"); err != nil || status != "pending" {
		t.Fatalf("first request = %q, %v", status, err)
	}
	first := state.friend
	if status, err := store.FriendRequest("alice", "bob", "portal"); err != nil || status != "pending" || state.friend != first {
		t.Fatalf("repeated request = %q, %v, friend=%+v", status, err, state.friend)
	}
	state.friend.status = "accepted"
	if status, err := store.FriendRequest("alice", "bob", "portal"); err != nil || status != "accepted" {
		t.Fatalf("accepted request = %q, %v", status, err)
	}
}

func TestFriendRequestRejectsSelfAndReservedNamespaces(t *testing.T) {
	store := newFriendTestStore(t, &friendState{users: map[string]bool{
		"alice": true, "portal": true, "infra": true,
	}})
	for _, tc := range []struct {
		name, requester, addressee, reserved string
	}{
		{"self", "alice", "alice", "infra"},
		{"literal portal", "alice", "portal", "infra"},
		{"configured portal", "alice", "infra", "infra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.FriendRequest(tc.requester, tc.addressee, tc.reserved); !errors.Is(err, ErrNamespaceConflict) {
				t.Fatalf("error = %v, want ErrNamespaceConflict", err)
			}
		})
	}
}

func TestReverseFriendRequestAutomaticallyAccepts(t *testing.T) {
	state := &friendState{users: map[string]bool{"alice": true, "bob": true}}
	store := newFriendTestStore(t, state)
	if status, err := store.FriendRequest("alice", "bob", "portal"); err != nil || status != "pending" {
		t.Fatalf("first request = %q, %v", status, err)
	}
	if status, err := store.FriendRequest("bob", "alice", "portal"); err != nil || status != "accepted" {
		t.Fatalf("reverse request = %q, %v", status, err)
	}
	if state.friend == nil || state.friend.status != "accepted" {
		t.Fatalf("friend state = %+v", state.friend)
	}
}

func TestOnlyAddresseeCanAcceptFriend(t *testing.T) {
	state := &friendState{
		users:  map[string]bool{"alice": true, "bob": true},
		friend: &friendRecord{id: 1, requester: "alice", addressee: "bob", status: "pending"},
	}
	store := newFriendTestStore(t, state)
	if err := store.FriendAccept("alice", "bob"); !errors.Is(err, ErrNoSuchFriend) {
		t.Fatalf("requester accepted own request: %v", err)
	}
	if err := store.FriendAccept("bob", "alice"); err != nil {
		t.Fatalf("addressee accept: %v", err)
	}
}

func TestFriendRemoveRevokesBothACLDirections(t *testing.T) {
	state := &friendState{
		users:  map[string]bool{"alice": true, "bob": true, "eve": true},
		friend: &friendRecord{id: 1, requester: "alice", addressee: "bob", status: "accepted"},
		acl: []friendACLRecord{
			{id: 1, owner: "alice", grantee: "bob"},
			{id: 2, owner: "bob", grantee: "alice"},
			{id: 3, owner: "alice", grantee: "eve"},
		},
	}
	store := newFriendTestStore(t, state)
	if err := store.FriendRemove("alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if state.friend != nil || !state.acl[0].revoked || !state.acl[1].revoked || state.acl[2].revoked {
		t.Fatalf("remove state: friend=%+v acl=%+v", state.friend, state.acl)
	}
}

func TestFriendRemoveWithoutRelationshipDoesNotTouchACL(t *testing.T) {
	state := &friendState{
		users: map[string]bool{"alice": true, "bob": true},
		acl:   []friendACLRecord{{id: 1, owner: "alice", grantee: "bob"}},
	}
	store := newFriendTestStore(t, state)
	if err := store.FriendRemove("alice", "bob"); !errors.Is(err, ErrNoSuchFriend) {
		t.Fatalf("remove error = %v", err)
	}
	if state.acl[0].revoked {
		t.Fatal("ACL was revoked without a relationship row")
	}
}

func TestAddACLRequiresAcceptedFriendAndRejectsSelf(t *testing.T) {
	state := &friendState{users: map[string]bool{"alice": true, "bob": true}}
	store := newFriendTestStore(t, state)
	if err := store.AddACL("alice", "dev", "bob", "exec,read"); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("non-friend AddACL error = %v", err)
	}
	if err := store.AddACL("alice", "dev", "alice", "exec,read"); !errors.Is(err, ErrNotFriends) {
		t.Fatalf("self AddACL error = %v", err)
	}
	state.friend = &friendRecord{id: 1, requester: "alice", addressee: "bob", status: "accepted"}
	if err := store.AddACL("alice", "dev", "bob", "read,exec"); err != nil {
		t.Fatal(err)
	}
	if len(state.acl) != 1 || state.acl[0].perms != "exec,read" {
		t.Fatalf("ACL = %+v", state.acl)
	}
}

func TestLookupUserIsExact(t *testing.T) {
	store := newFriendTestStore(t, &friendState{users: map[string]bool{"bob": true}})
	for _, tc := range []struct {
		namespace string
		want      bool
	}{{"bob", true}, {"Bob", false}, {"bo", false}} {
		got, err := store.LookupUser(tc.namespace)
		if err != nil || got != tc.want {
			t.Errorf("LookupUser(%q) = %v, %v; want %v", tc.namespace, got, err, tc.want)
		}
	}
}
