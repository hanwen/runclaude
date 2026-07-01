package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAllowlistAddMatch(t *testing.T) {
	a := NewAllowlist([]string{"api.anthropic.com"})
	if !a.Match("api.anthropic.com") {
		t.Fatal("seeded domain should match")
	}
	if a.Match("example.com") {
		t.Fatal("unseeded domain should not match")
	}
	if !a.Add("example.com") {
		t.Fatal("Add of new domain should report a change")
	}
	if !a.Match("example.com") {
		t.Fatal("added domain should match")
	}
	// Already-matched (exact and via wildcard) should be no-ops.
	if a.Add("example.com") {
		t.Fatal("re-adding an allowed domain should report no change")
	}
	a.Add("*.corp.internal")
	if a.Add("host.corp.internal") {
		t.Fatal("adding a domain already covered by a wildcard should be a no-op")
	}
}

func TestApproverRecordAndApprove(t *testing.T) {
	allow := NewAllowlist(nil)
	ap := NewApprover(allow, log.New(io.Discard, "", 0))

	ap.Record("pkg.example.com")
	ap.Record("pkg.example.com")
	ap.Record("other.example.com")

	denied := ap.Denied()
	if len(denied) != 2 {
		t.Fatalf("want 2 denied hosts, got %d", len(denied))
	}
	var pkg *DeniedEntry
	for i := range denied {
		if denied[i].Host == "pkg.example.com" {
			pkg = &denied[i]
		}
	}
	if pkg == nil || pkg.Count != 2 {
		t.Fatalf("pkg.example.com should have count 2, got %+v", pkg)
	}

	ap.Approve("pkg.example.com")
	if !allow.Match("pkg.example.com") {
		t.Fatal("approved host should be allowed")
	}
	for _, e := range ap.Denied() {
		if e.Host == "pkg.example.com" {
			t.Fatal("approved host should be cleared from the denied list")
		}
	}
}

func TestApproverHTTP(t *testing.T) {
	allow := NewAllowlist(nil)
	ap := NewApprover(allow, log.New(io.Discard, "", 0))
	ap.Record("pkg.example.com")
	srv := httptest.NewServer(ap)
	defer srv.Close()

	// Index lists the denied host.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "pkg.example.com") {
		t.Fatalf("index should list denied host:\n%s", body)
	}

	// POST /approve adds the host and redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.PostForm(srv.URL+"/approve", url.Values{"host": {"pkg.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve should redirect (303), got %d", resp.StatusCode)
	}
	if !allow.Match("pkg.example.com") {
		t.Fatal("host should be allowed after POST /approve")
	}

	// GET /approve is rejected.
	resp, err = http.Get(srv.URL + "/approve")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /approve should be 405, got %d", resp.StatusCode)
	}
}
