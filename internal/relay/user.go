package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"wanctl/internal/sessionauth"
)

func (r *Relay) registerUser(mux *http.ServeMux) {
	mux.HandleFunc("/u/friends", r.userFriends)
	mux.HandleFunc("/u/friends/request", r.userFriendRequest)
	mux.HandleFunc("/u/friends/accept", r.userFriendAccept)
	mux.HandleFunc("/u/friends/decline", r.userFriendDecline)
	mux.HandleFunc("/u/friends/remove", r.userFriendRemove)
	mux.HandleFunc("/u/users/lookup", r.userLookup)
	mux.HandleFunc("/u/shares", r.userShares)
	mux.HandleFunc("/u/shares/grant", r.userShareGrant)
	mux.HandleFunc("/u/shares/revoke", r.userShareRevoke)
}

func (r *Relay) requireUserStore(w http.ResponseWriter, req *http.Request) (string, bool) {
	namespace, ok := r.auth(w, req)
	if !ok {
		writeErrorToken(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if r.portalNS != "" && namespace == r.portalNS {
		writeErrorToken(w, http.StatusForbidden, "forbidden")
		return "", false
	}
	if r.admin == nil {
		http.Error(w, "Postgres admin store is not configured", http.StatusServiceUnavailable)
		return "", false
	}
	return namespace, true
}

func requireMethod(w http.ResponseWriter, req *http.Request, method string) bool {
	if req.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeErrorToken(w http.ResponseWriter, status int, token string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, token)
}

func friendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSuchUser):
		writeErrorToken(w, http.StatusNotFound, ErrNoSuchUser.Error())
	case errors.Is(err, ErrNoSuchFriend):
		writeErrorToken(w, http.StatusNotFound, ErrNoSuchFriend.Error())
	case errors.Is(err, ErrNamespaceConflict):
		writeErrorToken(w, http.StatusConflict, "invalid-friend")
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func decodeNamespace(req *http.Request) (string, error) {
	var body struct {
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return "", err
	}
	body.Namespace = strings.TrimSpace(body.Namespace)
	if body.Namespace == "" {
		return "", errors.New("namespace required")
	}
	return body.Namespace, nil
}

func (r *Relay) userFriends(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodGet) {
		return
	}
	r.listFriends(w, namespace)
}

func (r *Relay) listFriends(w http.ResponseWriter, namespace string) {
	friends, err := r.admin.ListFriends(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"friends": friends})
}

func (r *Relay) userFriendRequest(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	peer, err := decodeNamespace(req)
	if err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	status, err := r.admin.FriendRequest(namespace, peer, r.portalNS)
	if err != nil {
		friendError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": status})
}

func (r *Relay) userFriendAccept(w http.ResponseWriter, req *http.Request) {
	r.userFriendDecision(w, req, "accept")
}

func (r *Relay) userFriendDecline(w http.ResponseWriter, req *http.Request) {
	r.userFriendDecision(w, req, "decline")
}

func (r *Relay) userFriendRemove(w http.ResponseWriter, req *http.Request) {
	r.userFriendDecision(w, req, "remove")
}

func (r *Relay) userFriendDecision(w http.ResponseWriter, req *http.Request, action string) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	peer, err := decodeNamespace(req)
	if err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	switch action {
	case "accept":
		err = r.admin.FriendAccept(namespace, peer)
	case "decline":
		err = r.admin.FriendDecline(namespace, peer)
	case "remove":
		err = r.admin.FriendRemove(namespace, peer)
	}
	if err != nil {
		friendError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) userLookup(w http.ResponseWriter, req *http.Request) {
	_, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodGet) {
		return
	}
	r.lookupUser(w, strings.TrimSpace(req.URL.Query().Get("namespace")))
}

func (r *Relay) lookupUser(w http.ResponseWriter, namespace string) {
	if namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	exists, err := r.admin.LookupUser(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		writeErrorToken(w, http.StatusNotFound, ErrNoSuchUser.Error())
		return
	}
	writeJSON(w, map[string]string{"namespace": namespace})
}

func (r *Relay) userShares(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodGet) {
		return
	}
	givenRows, err := r.admin.ListACL(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	given := make([]map[string]any, 0, len(givenRows))
	for _, row := range givenRows {
		given = append(given, map[string]any{
			"id": row["id"], "device": row["device"],
			"grantee": row["grantee"], "perms": row["perms"],
		})
	}
	received, err := r.admin.ListReceivedACL(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"given": given, "received": received})
}

func (r *Relay) userShareGrant(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		Device  string `json:"device"`
		Grantee string `json:"grantee"`
		Perms   string `json:"perms"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.Device = strings.TrimSpace(body.Device)
	body.Grantee = strings.TrimSpace(body.Grantee)
	body.Perms = strings.TrimSpace(body.Perms)
	if body.Device == "" || body.Grantee == "" {
		http.Error(w, "device and grantee required", http.StatusBadRequest)
		return
	}
	if body.Perms == "" {
		body.Perms = "exec,read"
	}
	if _, err := sessionauth.ParseGrant(body.Perms); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := r.admin.GrantACL(namespace, body.Device, body.Grantee, body.Perms)
	if errors.Is(err, ErrNotFriends) {
		writeErrorToken(w, http.StatusForbidden, ErrNotFriends.Error())
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int{"id": id})
}

func (r *Relay) userShareRevoke(w http.ResponseWriter, req *http.Request) {
	namespace, ok := r.requireUserStore(w, req)
	if !ok || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		ID      int    `json:"id"`
		Device  string `json:"device"`
		Grantee string `json:"grantee"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.Device = strings.TrimSpace(body.Device)
	body.Grantee = strings.TrimSpace(body.Grantee)
	if body.ID <= 0 && (body.Device == "" || body.Grantee == "") {
		http.Error(w, "id or device and grantee required", http.StatusBadRequest)
		return
	}
	revoked, err := r.admin.RevokeACLMatch(namespace, body.ID, body.Device, body.Grantee)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !revoked {
		writeErrorToken(w, http.StatusNotFound, "not-found")
		return
	}
	w.WriteHeader(http.StatusOK)
}

type adminFriendBody struct {
	Namespace string `json:"namespace"`
	Peer      string `json:"peer"`
}

func (r *Relay) adminFriends(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodGet) {
		return
	}
	namespace := strings.TrimSpace(req.URL.Query().Get("namespace"))
	if namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	r.listFriends(w, namespace)
}

func (r *Relay) adminFriendRequest(w http.ResponseWriter, req *http.Request) {
	r.adminFriendAction(w, req, "request")
}

func (r *Relay) adminFriendAccept(w http.ResponseWriter, req *http.Request) {
	r.adminFriendAction(w, req, "accept")
}

func (r *Relay) adminFriendDecline(w http.ResponseWriter, req *http.Request) {
	r.adminFriendAction(w, req, "decline")
}

func (r *Relay) adminFriendRemove(w http.ResponseWriter, req *http.Request) {
	r.adminFriendAction(w, req, "remove")
}

func (r *Relay) adminFriendAction(w http.ResponseWriter, req *http.Request, action string) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body adminFriendBody
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.Namespace = strings.TrimSpace(body.Namespace)
	body.Peer = strings.TrimSpace(body.Peer)
	if body.Namespace == "" || body.Peer == "" {
		http.Error(w, "namespace and peer required", http.StatusBadRequest)
		return
	}
	var status string
	var err error
	switch action {
	case "request":
		status, err = r.admin.FriendRequest(body.Namespace, body.Peer, r.portalNS)
	case "accept":
		err = r.admin.FriendAccept(body.Namespace, body.Peer)
	case "decline":
		err = r.admin.FriendDecline(body.Namespace, body.Peer)
	case "remove":
		err = r.admin.FriendRemove(body.Namespace, body.Peer)
	}
	if err != nil {
		friendError(w, err)
		return
	}
	if action == "request" {
		writeJSON(w, map[string]string{"status": status})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) adminUserLookup(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodGet) {
		return
	}
	r.lookupUser(w, strings.TrimSpace(req.URL.Query().Get("namespace")))
}
