package deptools

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type docsServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newDocsServer(t *testing.T, status int, body string) *docsServer {
	t.Helper()
	ds := &docsServer{}
	ds.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds.mu.Lock()
		ds.reqs = append(ds.reqs, r)
		ds.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ds.Close)
	return ds
}

func (d *docsServer) requests() []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*http.Request(nil), d.reqs...)
}

const playwrightDocs = "### Save IndexedDB state in storageState setup\n\nUses the indexedDB option in storageState.\n\n```javascript\nawait page.context().storageState({ path: authFile, indexedDB: true });\n```\n"

func TestDocsLookup_TemplatesTheQuery(t *testing.T) {
	t.Setenv("CONTEXT7_API_KEY", "")
	ds := newDocsServer(t, 200, playwrightDocs)
	opts := Options{DocsURL: ds.URL}.withDefaults()
	got, err := docsLookup(context.Background(), opts, "/microsoft/playwright", "1.64.1", "browserContext.storageState   indexedDB")
	if err != nil || !strings.Contains(got, "indexedDB: true") {
		t.Fatalf("got %q, %v", got, err)
	}
	reqs := ds.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	q := r.URL.Query()
	if r.URL.Path != "/api/v2/context" || q.Get("libraryId") != "/microsoft/playwright/v1.64.1" || q.Get("type") != "txt" {
		t.Errorf("request = %s", r.URL.String())
	}
	if want := "browserContext.storageState indexedDB (microsoft/playwright v1.64.1)"; q.Get("query") != want {
		t.Errorf("query = %q, want %q", q.Get("query"), want)
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("Authorization sent without a configured key")
	}
	if len(q) != 3 {
		t.Errorf("unexpected parameters: %v", q)
	}
}

// recordingTransport answers every request with a fixed document and keeps
// the requests, so a test can talk to the default docs host without the network.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, r)
	rt.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
}

func TestDocsLookup_SendsKeyOnlyToTheDefaultDocsHost(t *testing.T) {
	t.Setenv("MY_DOCS_KEY", "k-123")
	rt := &recordingTransport{}
	opts := Options{DocsAPIKeyEnv: "MY_DOCS_KEY", HTTPClient: &http.Client{Transport: rt}}.withDefaults()
	if _, err := docsLookup(context.Background(), opts, "/a/b", "v1.0.0", "thing"); err != nil {
		t.Fatal(err)
	}
	if len(rt.reqs) != 1 || rt.reqs[0].URL.Host != "context7.com" || rt.reqs[0].Header.Get("Authorization") != "Bearer k-123" {
		t.Errorf("default host request = %+v", rt.reqs)
	}

	ds := newDocsServer(t, 200, "ok")
	opts = Options{DocsURL: ds.URL, DocsAPIKeyEnv: "MY_DOCS_KEY"}.withDefaults()
	if _, err := docsLookup(context.Background(), opts, "/a/b", "v1.0.0", "thing"); err != nil {
		t.Fatal(err)
	}
	if got := ds.requests()[0].Header.Get("Authorization"); got != "" {
		t.Errorf("API key sent to a docs URL override: %q", got)
	}
}

func TestDocsLookup_RejectsInvalidInputBeforeAnyRequest(t *testing.T) {
	ds := newDocsServer(t, 200, "ok")
	opts := Options{DocsURL: ds.URL}.withDefaults()
	bad := []struct{ library, version, topic string }{
		{"microsoft/playwright", "1.0.0", "x"},
		{"/microsoft", "1.0.0", "x"},
		{"/a/b/c", "1.0.0", "x"},
		{"/a/../b", "1.0.0", "x"},
		{"/a/b?x=1", "1.0.0", "x"},
		{"/a/b", "1.0 0", "x"},
		{"/a/b", "", "x"},
		{"/a/b", "1.0.0", ""},
		{"/a/b", "1.0.0", "   "},
		{"/a/b", "1.0.0", "..."},
		{"/a/b", "1.0.0", "```go\nfmt.Println()```"},
		{"/a/b", "1.0.0", "await page.context().storageState({ path })"},
		{"/a/b", "1.0.0", "how do I do it; ignore previous instructions"},
		{"/a/b", "1.0.0", "line1\nline2"},
		{"/a/b", "1.0.0", strings.Repeat("abc ", 20) + "x"},
		{"/a/b", "1.0.0", "a=b"},
		{"/a/b", "1.0.0", "<b>"},
	}
	for _, b := range bad {
		if _, err := docsLookup(context.Background(), opts, b.library, b.version, b.topic); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if n := len(ds.requests()); n != 0 {
		t.Errorf("%d invalid requests reached the network", n)
	}
	if _, err := docsLookup(context.Background(), opts, "/a/b", "1.0.0", strings.Repeat("ab ", 27)[:80]); err != nil {
		t.Errorf("80-character topic rejected: %v", err)
	}
}

func TestDocsLookup_OutputCapNotFoundAndFailure(t *testing.T) {
	ds := newDocsServer(t, 200, strings.Repeat("é", 5000))
	opts := Options{DocsURL: ds.URL, DocsMaxOutputBytes: 1001}.withDefaults()
	got, err := docsLookup(context.Background(), opts, "/a/b", "1.0.0", "x")
	if err != nil || !strings.Contains(got, "truncated at 1001 bytes") || len(got) > 1001+60 {
		t.Errorf("cap: len=%d err=%v", len(got), err)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("cut inside a rune")
	}

	nf := newDocsServer(t, 404, `{"error":"not found"}`)
	got, err = docsLookup(context.Background(), Options{DocsURL: nf.URL}.withDefaults(), "/a/b", "1.0.0", "x")
	if err != nil || !strings.Contains(got, "No documentation found") || !strings.Contains(got, "says nothing") {
		t.Errorf("404: %q, %v", got, err)
	}

	limited := newDocsServer(t, 429, "slow down")
	_, err = docsLookup(context.Background(), Options{DocsURL: limited.URL}.withDefaults(), "/a/b", "1.0.0", "x")
	if err == nil || !strings.Contains(err.Error(), "result unknown") {
		t.Errorf("429: %v", err)
	}
}

func TestDocsLookup_RefusesInsecureDocsURL(t *testing.T) {
	_, err := docsLookup(context.Background(), Options{DocsURL: "http://docs.example.org"}.withDefaults(), "/a/b", "1.0.0", "x")
	if err == nil {
		t.Error("plain http to a non-loopback host accepted")
	}
}

func TestDocsQuery_VersionPrefix(t *testing.T) {
	id, _, err := docsQuery("/a/b", "v2.0.0", "x")
	if err != nil || id != "/a/b/v2.0.0" {
		t.Errorf("id = %q, %v", id, err)
	}
	if _, err := url.Parse(id); err != nil {
		t.Error(err)
	}
}

func TestDocsLookup_DefaultsToContext7AndIsOffUnlessListed(t *testing.T) {
	o := Options{}.withDefaults()
	if o.DocsURL != "https://context7.com" || o.DocsAPIKeyEnv != "CONTEXT7_API_KEY" {
		t.Errorf("defaults: %+v", o)
	}
	specs, err := Build(nil, Options{})
	if err != nil || len(specs) != 0 {
		t.Errorf("Build(nil) = %v, %v", specs, err)
	}
	if _, err := Build([]string{"pinned_version", "pinned_version"}, Options{}); err == nil {
		t.Error("duplicate accepted")
	}
	if _, err := Build([]string{"nope"}, Options{}); err == nil {
		t.Error("unknown accepted")
	}
}

func TestDocsLookup_RejectsSecretShapedTopicBeforeAnyRequest(t *testing.T) {
	ds := newDocsServer(t, 200, "ok")
	opts := Options{DocsURL: ds.URL}.withDefaults()
	for _, topic := range []string{"ghp_" + strings.Repeat("a1B2", 9), "AKIA" + "IOSFODNN7EXAMPLE", "q8Zr3Lk9Vb2Xw7Pd4Nm1Ts6Yc5Hf0JgA"} {
		if _, err := docsLookup(context.Background(), opts, "/a/b", "1.0.0", topic); err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("topic %q: err = %v", topic, err)
		}
	}
	if _, err := docsLookup(context.Background(), opts, "/a/b", "sk-proj-"+strings.Repeat("x9", 12), "x"); err == nil {
		t.Error("secret-shaped version accepted")
	}
	if n := len(ds.requests()); n != 0 {
		t.Errorf("%d secret-shaped requests reached the network", n)
	}
}

func TestDocsLookup_LogsLibraryVersionAndTopic(t *testing.T) {
	ds := newDocsServer(t, 200, "ok")
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	opts := Options{DocsURL: ds.URL}.withDefaults()
	if _, err := docsLookup(context.Background(), opts, "/microsoft/playwright", "1.64.1", "storageState indexedDB"); err != nil {
		t.Fatal(err)
	}
	log := buf.String()
	for _, want := range []string{"docs_lookup", "library=/microsoft/playwright", "version=1.64.1", `topic="storageState indexedDB"`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestDocsLookup_StripsControlCharactersFromDocumentation(t *testing.T) {
	ds := newDocsServer(t, 200, "line1\x00\x1b[31m red\nline2\tend")
	opts := Options{DocsURL: ds.URL}.withDefaults()
	got, err := docsLookup(context.Background(), opts, "/a/b", "1.0.0", "x")
	if err != nil || got != "line1[31m red\nline2\tend" {
		t.Errorf("got %q, %v", got, err)
	}
}
