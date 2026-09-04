package watch

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testWebhookID = "0123456789abcdef0123456789abcdef"

// testNotifier builds a notifier pointing at srv that does not really wait
// between attempts; the log goes to buf so a test can check what ends up in it.
func testNotifier(t *testing.T, targetURL string) (*Notifier, *bytes.Buffer) {
	t.Helper()
	u, err := url.Parse(targetURL)
	if err != nil {
		t.Fatalf("test URL cannot be parsed: %v", err)
	}
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := &Notifier{
		url:      u,
		client:   &http.Client{Timeout: 5 * time.Second},
		log:      log,
		Backoffs: []time.Duration{0, 0}, // no real backoff in a test
	}
	return m, buf
}

func TestSendRetriesOn500(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m, _ := testNotifier(t, srv.URL+"/api/webhook/"+testWebhookID)
	err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"})
	if err == nil {
		t.Fatal("a 500 produced no error")
	}
	if got := atomic.LoadInt32(&attempts); got != maxAttempts {
		t.Errorf("number of attempts = %d, want %d", got, maxAttempts)
	}
}

func TestSendStopsImmediatelyOn404(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "webhook does not exist", http.StatusNotFound)
	}))
	defer srv.Close()

	m, buf := testNotifier(t, srv.URL+"/api/webhook/"+testWebhookID)
	err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"})
	if err == nil {
		t.Fatal("a 404 produced no error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("number of attempts = %d, want 1: a 4xx is permanent", got)
	}
	if !strings.Contains(err.Error(), "permanent error") {
		t.Errorf("error was not marked as permanent: %v", err)
	}
	// The failure path may not leak the webhook id either.
	if strings.Contains(err.Error(), testWebhookID) || strings.Contains(buf.String(), testWebhookID) {
		t.Error("the webhook id appears in the error or in the log")
	}
}

func TestSendSucceedsAndLeaksNoID(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("request not readable: %v", err)
		}
		received = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, buf := testNotifier(t, srv.URL+"/api/webhook/"+testWebhookID)
	// A file name with quotes and a backslash: exactly why this program is
	// written in Go and not in bash with jq.
	awkward := `/mnt/tank/photos/2020/robin's "holiday" \0040 dir/photo".jpg`
	p := Payload{
		Version:  1,
		Status:   "ok",
		Deleted:  2,
		Examples: []string{awkward},
	}
	if err := m.Send(context.Background(), p); err != nil {
		t.Fatalf("sending failed: %v", err)
	}

	var back Payload
	if err := json.Unmarshal(received, &back); err != nil {
		t.Fatalf("the received JSON cannot be parsed: %v (%s)", err, received)
	}
	if len(back.Examples) != 1 || back.Examples[0] != awkward {
		t.Errorf("path arrived mangled: %q", back.Examples)
	}
	if strings.Contains(buf.String(), testWebhookID) {
		t.Errorf("the webhook id appears in a log line: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "/api/webhook/…") {
		t.Errorf("the masked URL is missing from the log: %s", buf.String())
	}
}

func TestSendEmptyExamplesBecomesEmptyList(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("request not readable: %v", err)
		}
		received = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := testNotifier(t, srv.URL+"/api/webhook/"+testWebhookID)
	if err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"}); err != nil {
		t.Fatalf("sending failed: %v", err)
	}
	if !bytes.Contains(received, []byte(`"examples":[]`)) {
		t.Errorf("examples did not become an empty list: %s", received)
	}
}

// HA_IP is the emergency brake for when the storage host cannot resolve the
// Home Assistant name. This test shows that the connection goes to the IP while
// the URL (and therefore the Host header, and over https the name the
// certificate is checked against) stays that of the host name.
func TestHAIPConnectsToTheIPButKeepsTheHostname(t *testing.T) {
	var seenHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	real, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("server URL cannot be parsed: %v", err)
	}
	// A name that deliberately resolves nowhere: if it does resolve, the test is
	// wrong instead of silently right.
	invented := &url.URL{Scheme: "http", Host: "photowatch.invalid:" + real.Port(), Path: "/api/webhook/" + testWebhookID}

	cfg := &Config{WebhookURL: invented, HAIP: "127.0.0.1"}
	buf := &bytes.Buffer{}
	m := NewNotifier(cfg, slog.New(slog.NewTextHandler(buf, nil)))
	m.Backoffs = []time.Duration{0, 0}

	if err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"}); err != nil {
		t.Fatalf("sending through HA_IP failed: %v", err)
	}
	if seenHost != invented.Host {
		t.Errorf("Host header = %q, want %q; the host name must be preserved, otherwise the certificate check fails", seenHost, invented.Host)
	}
}

// The core of the HA_IP detour is that *only* the address changes: the TLS
// handshake must still use the host name from the URL, otherwise certificate
// verification would have to be turned off, and that never happens here. This
// test proves it with a real TLS server: the httptest certificate is issued for
// example.com, the connection goes to 127.0.0.1, and the handshake only
// succeeds when the ServerName is example.com.
func TestHAIPKeepsTheTLSHostnameFromTheURL(t *testing.T) {
	var seenHost, seenServerName string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		if r.TLS != nil {
			seenServerName = r.TLS.ServerName
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	real, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("server URL cannot be parsed: %v", err)
	}
	// example.com is the name on the httptest test certificate; here it plays
	// the role of the Home Assistant host name.
	target := &url.URL{Scheme: "https", Host: "example.com:" + real.Port(), Path: "/api/webhook/" + testWebhookID}

	buf := &bytes.Buffer{}
	m := NewNotifier(&Config{WebhookURL: target, HAIP: "127.0.0.1"}, slog.New(slog.NewTextHandler(buf, nil)))
	m.Backoffs = []time.Duration{0, 0}
	// Trust only the root of the test server. InsecureSkipVerify is deliberately
	// absent: with it, the test would no longer prove what it has to prove.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	m.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool

	if err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"}); err != nil {
		t.Fatalf("sending through HA_IP over TLS failed: %v", err)
	}
	if seenServerName != "example.com" {
		t.Errorf("TLS ServerName = %q, want example.com; the certificate must be checked against the host name from the URL, not against the IP", seenServerName)
	}
	if seenHost != target.Host {
		t.Errorf("Host header = %q, want %q", seenHost, target.Host)
	}
	if strings.Contains(buf.String(), testWebhookID) {
		t.Errorf("the webhook id appears in a log line: %s", buf.String())
	}
}

// The easiest way for the id to leak: a network error. net/http puts the full
// URL into *url.Error, so that text may never land unedited in an error message
// or a log line.
func TestNetworkErrorDoesNotLeakTheID(t *testing.T) {
	// .invalid is by convention (RFC 2606) a domain that never exists; the
	// lookup therefore fails at once and the test needs no network.
	target := &url.URL{Scheme: "https", Host: "photowatch.invalid:8123", Path: "/api/webhook/" + testWebhookID}
	buf := &bytes.Buffer{}
	m := NewNotifier(&Config{WebhookURL: target}, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	m.Backoffs = []time.Duration{0, 0}

	err := m.Send(context.Background(), Payload{Version: 1, Status: "ok"})
	if err == nil {
		t.Fatal("an unreachable host produced no error")
	}
	if strings.Contains(err.Error(), testWebhookID) {
		t.Errorf("the webhook id appears in the error: %v", err)
	}
	if strings.Contains(buf.String(), testWebhookID) {
		t.Errorf("the webhook id appears in the log: %s", buf.String())
	}
	if !strings.Contains(err.Error(), "photowatch.invalid") {
		t.Errorf("the error no longer says *which* host went wrong: %v", err)
	}
}

func TestMaskURL(t *testing.T) {
	cases := map[string]string{
		// Without a trailing slash: the ordinary case.
		"https://home.example.net:8123/api/webhook/" + testWebhookID: "https://home.example.net:8123/api/webhook/…",
		// *With* a trailing slash. This went wrong before: cutting at the last /
		// leaves an empty last segment and puts the id in the log in full.
		"https://home.example.net:8123/api/webhook/" + testWebhookID + "/": "https://home.example.net:8123/api/webhook/…/",
		"https://home.example.net:8123/" + testWebhookID:                   "https://home.example.net:8123/…",
		// No path at all: there is nothing to mask, but nothing may leak either.
		"https://home.example.net:8123": "https://home.example.net:8123…",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("URL cannot be parsed: %v", err)
		}
		got := MaskURL(u)
		if got != want {
			t.Errorf("MaskURL(%q) = %q, want %q", raw, got, want)
		}
		if strings.Contains(got, testWebhookID) {
			t.Errorf("the id is still in the masked URL: %s", got)
		}
	}
	if MaskURL(nil) != "(no url)" {
		t.Error("an empty URL must give readable text, not a panic")
	}
}

// maskError removes the id from the text of an error from net/http. That only
// works when the last *non-empty* segment is taken.
func TestMaskErrorAlsoWithTrailingSlash(t *testing.T) {
	for _, raw := range []string{
		"https://home.example.net:8123/api/webhook/" + testWebhookID,
		"https://home.example.net:8123/api/webhook/" + testWebhookID + "/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("URL cannot be parsed: %v", err)
		}
		fake := &url.Error{Op: "Post", URL: raw, Err: errors.New("connection refused")}
		if got := maskError(fake, u); strings.Contains(got, testWebhookID) {
			t.Errorf("the webhook id is still in the masked error: %s", got)
		}
	}
}

func TestExamplesTruncatesAndLimits(t *testing.T) {
	var paths []DeletedFile
	for i := 0; i < 12; i++ {
		path := "/mnt/tank/photos/" + strings.Repeat("a", 200)
		paths = append(paths, DeletedFile{Raw: path, Path: path, Decodable: true})
	}
	v := Examples(paths)
	if len(v) != maxExamples {
		t.Errorf("number of examples = %d, want %d", len(v), maxExamples)
	}
	for _, s := range v {
		if n := len([]rune(s)); n > maxExampleLength+1 { // +1 for the ellipsis
			t.Errorf("example is %d characters, want at most %d", n, maxExampleLength+1)
		}
	}
	if Examples(nil) == nil {
		t.Error("Examples(nil) must give an empty list, not nil")
	}
}
