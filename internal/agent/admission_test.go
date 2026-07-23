package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeregisterHTTPUsesBearerHeaderWithoutTokenURL(t *testing.T) {
	const token = "deregister-secret"
	var requestURI, authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestURI = req.RequestURI
		authorization = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := &Agent{opts: Options{Token: token, Name: "home-pc"}, inst: "instance-a"}
	a.deregisterHTTP(srv.URL)
	if strings.Contains(requestURI, token) || strings.Contains(requestURI, "token=") {
		t.Fatal("credential appeared in deregister request URI")
	}
	if authorization != "Bearer "+token {
		t.Fatal("deregister request did not carry the expected bearer credential")
	}
}
