package proxy

// Runtime domain approval. The proxy starts with a fixed allowlist, but a
// long-running sandboxed process routinely reaches for a host nobody thought
// to allow up front (a new package mirror, a docs site, an API). Rather than
// killing and restarting with an extra --allow-domain, the host can approve
// the domain live: the Approver records every denied egress attempt and
// serves a small localhost web UI that lists them with an "approve" button.
// Approving adds the host to the shared Allowlist, so both the CONNECT/HTTP
// proxy and the in-container DNS server permit it immediately.

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Allowlist is a concurrency-safe, growable set of domain patterns. A single
// instance backs both the proxy's allow decision (Serve) and the DNS server,
// so a runtime approval takes effect for name resolution and egress at once.
type Allowlist struct {
	mu    sync.RWMutex
	items []string
}

// NewAllowlist returns an Allowlist seeded with initial (copied).
func NewAllowlist(initial []string) *Allowlist {
	return &Allowlist{items: append([]string(nil), initial...)}
}

// Match reports whether host matches any pattern currently in the list.
func (a *Allowlist) Match(host string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return MatchDomain(host, a.items)
}

// Add inserts pattern unless it is already matched. It reports whether the
// list changed.
func (a *Allowlist) Add(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if MatchDomain(strings.ToLower(pattern), a.items) {
		return false
	}
	a.items = append(a.items, pattern)
	return true
}

// Snapshot returns a copy of the current patterns.
func (a *Allowlist) Snapshot() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]string(nil), a.items...)
}

// DeniedEntry aggregates the egress attempts to one host that the allowlist
// rejected, for display in the approval UI.
type DeniedEntry struct {
	Host  string
	Count int
	First time.Time
	Last  time.Time
}

// Approver records denied egress attempts and serves a localhost web UI that
// lists them with an "approve" button. Approving a host adds it to the
// underlying Allowlist. The zero value is not usable; use NewApprover.
type Approver struct {
	allow  *Allowlist
	logger *log.Logger
	now    func() time.Time

	mu     sync.Mutex
	denied map[string]*DeniedEntry
}

// NewApprover returns an Approver that grants approvals by adding to allow.
func NewApprover(allow *Allowlist, logger *log.Logger) *Approver {
	return &Approver{
		allow:  allow,
		logger: logger,
		now:    time.Now,
		denied: map[string]*DeniedEntry{},
	}
}

// Record notes a denied request to host. It is wired into Serve via
// Setup.OnDeny and is safe for concurrent use.
func (ap *Approver) Record(host string) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	e := ap.denied[host]
	now := ap.now()
	if e == nil {
		e = &DeniedEntry{Host: host, First: now}
		ap.denied[host] = e
	}
	e.Count++
	e.Last = now
}

// Approve adds host to the allowlist and clears it from the denied list. It
// reports whether host was a usable value.
func (ap *Approver) Approve(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	added := ap.allow.Add(host)
	ap.mu.Lock()
	delete(ap.denied, host)
	ap.mu.Unlock()
	if ap.logger != nil {
		if added {
			ap.logger.Printf("approve %s (added to allowlist)", host)
		} else {
			ap.logger.Printf("approve %s (already allowed)", host)
		}
	}
	return true
}

// Denied returns the current denied entries, most-recently-seen first.
func (ap *Approver) Denied() []DeniedEntry {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	out := make([]DeniedEntry, 0, len(ap.denied))
	for _, e := range ap.denied {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// ServeHTTP implements the approval web UI: GET / lists denied hosts and the
// current allowlist; POST /approve?host=... (also accepts a form field)
// approves a host and redirects back.
func (ap *Approver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/approve":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ap.Approve(r.FormValue("host"))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "/":
		ap.renderIndex(w)
	default:
		http.NotFound(w, r)
	}
}

func (ap *Approver) renderIndex(w http.ResponseWriter) {
	denied := ap.Denied()
	allowed := ap.allow.Snapshot()
	sort.Strings(allowed)

	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">`)
	b.WriteString(`<meta http-equiv="refresh" content="3">`)
	b.WriteString(`<title>runclaude &mdash; domain approval</title>`)
	b.WriteString(`<style>` + approvalCSS + `</style></head><body>`)
	b.WriteString(`<h1>runclaude domain approval</h1>`)

	b.WriteString(`<h2>Denied requests</h2>`)
	if len(denied) == 0 {
		b.WriteString(`<p class="empty">No denied requests yet.</p>`)
	} else {
		b.WriteString(`<table><tr><th>Host</th><th>Attempts</th><th>Last seen</th><th></th></tr>`)
		for _, e := range denied {
			fmt.Fprintf(&b,
				`<tr><td class="host">%s</td><td>%d</td><td>%s</td>`+
					`<td><form method="post" action="/approve">`+
					`<input type="hidden" name="host" value="%s">`+
					`<button type="submit">approve</button></form></td></tr>`,
				html.EscapeString(e.Host), e.Count,
				html.EscapeString(e.Last.Format("15:04:05")),
				html.EscapeString(e.Host))
		}
		b.WriteString(`</table>`)
	}

	b.WriteString(`<h2>Approve a domain</h2>`)
	b.WriteString(`<form method="post" action="/approve" class="add">` +
		`<input type="text" name="host" placeholder="example.com or *.example.com" size="40">` +
		`<button type="submit">approve</button></form>`)

	b.WriteString(`<h2>Allowed domains</h2><ul class="allowed">`)
	for _, d := range allowed {
		fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(d))
	}
	b.WriteString(`</ul></body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, b.String())
}

const approvalCSS = `
body{font-family:system-ui,sans-serif;margin:2rem auto;max-width:48rem;color:#222}
h1{font-size:1.4rem}h2{font-size:1.05rem;margin-top:1.6rem;border-bottom:1px solid #ddd;padding-bottom:.2rem}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.35rem .6rem;border-bottom:1px solid #eee}
.host{font-family:ui-monospace,monospace}
button{cursor:pointer;padding:.25rem .8rem}
.empty{color:#777}
.add input{padding:.3rem}
.allowed{columns:2;font-family:ui-monospace,monospace;font-size:.85rem;color:#555}
`
