package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// One attempt may take this long at most. HA answers a webhook within a
	// fraction of that; if it is slower, something is wrong and waiting no
	// longer helps within the same run.
	attemptTimeout = 10 * time.Second
	maxAttempts    = 3
	// At most 5 examples of 120 characters each: a notification has to fit on a
	// phone screen, the full list is in the report.
	maxExamples      = 5
	maxExampleLength = 120
	// At most three folder names in the notification, each cut off at 60
	// characters.
	maxFolders          = 3
	maxFolderNameLength = 60
	// From the response we only read the status code; the body is read up to
	// here at most, to turn it into a log line on failure.
	maxResponseBody = 1024
)

// defaultBackoffs: the pauses between attempts. With three attempts only two
// pauses are needed. A fourth attempt is deliberately absent: the run would
// then sit waiting for well over a minute while the dead man's switch in HA
// picks up the problem anyway.
var defaultBackoffs = []time.Duration{5 * time.Second, 15 * time.Second}

// ErrPermanent marks problems where retrying is pointless: a 4xx means a wrong
// webhook id or an automation that disappeared from HA.
var ErrPermanent = errors.New("permanent error")

// Payload is exactly what goes to the HA webhook. The field names are the keys
// the template sensors in HA are waiting for; renaming one therefore also means
// changing template-photowatch.yaml.
type Payload struct {
	Version int `json:"version"`
	// Time is the moment this notification was built, in RFC3339 with
	// nanoseconds. It is here for one reason, and that reason is worth writing
	// down: without a field that is guaranteed to change per run, the payload of
	// a failure that repeats identically every day is literally the same
	// message. The sensor in Home Assistant then gets the same state *and* the
	// same attributes, and then there is no state change for the alert
	// automation to fire on — day 1 a notification, day 2 through 60 silence,
	// while the heartbeat keeps running. Nanoseconds and not seconds, so that
	// two runs within the same second differ as well.
	Time             string `json:"time"`
	Status           string `json:"status"` // "ok" or "error"
	Dataset          string `json:"dataset"`
	PreviousSnapshot string `json:"previous_snapshot"`
	NewSnapshot      string `json:"new_snapshot"`
	Since            string `json:"since"`
	Deleted          int    `json:"deleted"`
	DeletedDirs      int    `json:"deleted_dirs"`
	DeletedOther     int    `json:"deleted_other"`
	Renamed          int    `json:"renamed"`
	Added            int    `json:"added"`
	// Lines from `zfs diff` that did not match the expected shape. Should always
	// be 0; when it is not, the output of zfs has changed and the count may no
	// longer be right. That is why it travels to HA and not only to the report.
	Unparsed int `json:"unparsed"`
	// Lines that fell outside PATH_PREFIX. A number by itself says nothing here
	// — a dataset holds more than the photo directory — but it does belong in
	// HA and not only in the report: together with the other counters it shows
	// afterwards how much there was to see that day. That PATH_PREFIX no longer
	// matches anything is not decided here but when the run starts
	// (checkPathPrefix in main.go); see the explanation there.
	Skipped   int      `json:"skipped"`
	Threshold int      `json:"threshold"`
	Alert     bool     `json:"alarm"`
	Examples  []string `json:"examples"`
	Report    string   `json:"report"`
	Message   string   `json:"message,omitempty"`
	DurationS float64  `json:"duration_s"`

	// Media against sidecars: "43 files gone" says little, "22 photos with their
	// 21 XMP sidecars" says everything. Both count towards the threshold.
	MediaFiles   int `json:"media_files"`
	SidecarFiles int `json:"sidecar_files"`
	// At most three folders with the most deletions, relative to the mountpoint.
	// This is what almost always answers the question "was this an accident?",
	// and it costs nothing: it is already in the paths we have anyway.
	Folders []string `json:"folders"`
	// The date on which the snapshot disappears and restoring is no longer
	// possible. Without that date nobody knows there is any hurry.
	RecoverUntil  string `json:"recover_until"`
	RestoreScript string `json:"restore_script"`
	// Thumbnails: the path to the day directory on the share. Deliberately no
	// *image* travels to Home Assistant: a directory with thumbnails of exactly
	// the deleted photos is the most sensitive directory there is, and the easy
	// route in HA (/local/…) is served without authentication.
	Thumbnails string `json:"thumbnails"`
	// How many thumbnails will be made for this run. An intention and not a
	// count, and that is on purpose: the scaling only happens after this message
	// has been sent (see runAftercare in main.go), so at this moment there is no
	// image yet. The path above is already fixed. How many were actually made is
	// in the journal ("aftercare done") and in status.json under
	// aftercare.thumbnails.
	ThumbnailsPlanned int `json:"thumbnails_planned"`
	// What went wrong in the aftercare: thumbnails, restore script or the
	// cleanup. Deliberately one field and not three: it is one line on a phone
	// screen, and it may never drown out the real message ("43 files are gone").
	// What the *previous* run ran into only after its notification is in here as
	// well, with "on the previous run" in front of it.
	SideIssues string `json:"side_issues,omitempty"`
	// The total size of the report directory plus the thumbnail directory.
	// Should level off after the first two weeks; if it keeps growing, the
	// cleanup is not working. A separate automation in HA complains above 800
	// MB.
	//
	// The number comes from the aftercare of the *previous* run and is therefore
	// a day old. Measuring it is a directory walk over THUMBNAIL_DIR, and that
	// should not happen before the alert: if that directory ever points at a
	// network mount, a hung mount would hold up the notification until
	// TimeoutStartSec. Besides, the number is more honest after the cleanup. For
	// a trend over weeks a day of age is nothing; after the very first run it is
	// 0.
	ArtifactsMB int `json:"artifacts_mb"`
}

// newPayload sets the fields that work the same in *every* notification. It
// exists mostly for Time: that field must be in every message, including the
// one from the OnFailure path, otherwise a daily failure repeats as an
// identical message that Home Assistant no longer reacts to.
func newPayload(now time.Time) Payload {
	return Payload{
		Version: 1,
		Time:    now.Format(time.RFC3339Nano),
		// Empty lists and not nil: json.Marshal turns a nil slice into `null`,
		// and on the HA side `| default([])` does not help against that — that
		// only catches a missing key, not a key with the value null. The
		// attribute would become the *text* "None", and that is truthy in Jinja.
		// The notification would then read "Mostly from: None."
		Examples: []string{},
		Folders:  []string{},
	}
}

// Notifier sends the payload to Home Assistant.
type Notifier struct {
	url      *url.URL
	client   *http.Client
	log      *slog.Logger
	Backoffs []time.Duration // configurable so that the tests do not really wait
}

// NewNotifier builds the HTTP client. When HA_IP is set, it connects to that IP
// but the certificate stays checked against the host name from the URL:
// http.Transport takes the ServerName for the TLS handshake from the request
// URL and not from the address we dial. That is why InsecureSkipVerify is not
// needed here — and therefore does not appear anywhere in this file.
func NewNotifier(cfg *Config, log *slog.Logger) *Notifier {
	dialer := &net.Dialer{Timeout: attemptTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		// No proxy from the environment: HA sits on the same subnet, and an
		// http_proxy variable that accidentally lands in the unit's environment
		// must not silently reroute this traffic.
		Proxy: nil,
	}
	if cfg.HAIP != "" {
		ip := cfg.HAIP
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("address %q cannot be split: %w", address, err)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		}
	} else {
		tr.DialContext = dialer.DialContext
	}
	return &Notifier{
		url:      cfg.WebhookURL,
		client:   &http.Client{Transport: tr, Timeout: attemptTimeout},
		log:      log,
		Backoffs: defaultBackoffs,
	}
}

// Send does the POST, with retries on network errors and 5xx.
func (m *Notifier) Send(ctx context.Context, p Payload) error {
	// json.Marshal turns a nil slice into `null`, and on the HA side that
	// becomes the text "None" instead of an empty list; see newPayload.
	if p.Examples == nil {
		p.Examples = []string{}
	}
	if p.Folders == nil {
		p.Folders = []string{}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("payload cannot be encoded as JSON: %w", err)
	}

	target := MaskURL(m.url)
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		code, response, err := m.oneAttempt(ctx, body)
		switch {
		case err == nil && code >= 200 && code < 300:
			m.log.Info("notification sent to Home Assistant",
				"target", target, "http_status", code, "attempt", attempt, "status", p.Status, "alert", p.Alert)
			return nil
		case err == nil && code >= 400 && code < 500:
			// Permanent: retrying changes nothing about a request that is
			// refused. Note what a 4xx does and does not mean here: a *wrong*
			// webhook id gives no 4xx — Home Assistant answers 200 to an unknown
			// id on purpose, so that ids cannot be probed. A 4xx therefore comes
			// from something else: a reverse proxy in front of it, a wrong path,
			// or allowed_methods in the template that does not permit POST
			// (405).
			return fmt.Errorf("%w: got %d back from %s (%s); "+
				"this is not a wrong webhook id (that gives 200) but a refused request: "+
				"check the path /api/webhook/... in WEBHOOK_URL, allowed_methods in the template, "+
				"and whether there is a proxy in front of Home Assistant",
				ErrPermanent, code, target, response)
		case err == nil:
			last = fmt.Errorf("Home Assistant answered %d on %s (%s)", code, target, response)
		default:
			last = err
		}

		if ctx.Err() != nil {
			return fmt.Errorf("notifying aborted: %w (last error: %v)", ctx.Err(), last)
		}
		if attempt < maxAttempts {
			wait := m.backoff(attempt)
			m.log.Warn("notification failed, retrying",
				"target", target, "attempt", attempt, "of", maxAttempts, "wait_s", wait.Seconds(), "error", last)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return fmt.Errorf("notifying aborted while waiting: %w (last error: %v)", ctx.Err(), last)
			}
		}
	}
	return fmt.Errorf("notification to %s failed after %d attempts: %w", target, maxAttempts, last)
}

func (m *Notifier) backoff(attempt int) time.Duration {
	if len(m.Backoffs) == 0 {
		return 0
	}
	if attempt-1 < len(m.Backoffs) {
		return m.Backoffs[attempt-1]
	}
	return m.Backoffs[len(m.Backoffs)-1]
}

func (m *Notifier) oneAttempt(ctx context.Context, reqBody []byte) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url.String(), bytes.NewReader(reqBody))
	if err != nil {
		return 0, "", fmt.Errorf("request to %s cannot be built: %w", MaskURL(m.url), err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "photowatch/1")

	resp, err := m.client.Do(req)
	if err != nil {
		// The URL in the error from net/http holds the webhook id; hence the
		// masked version here and only the bare reason behind it.
		return 0, "", fmt.Errorf("connecting to %s failed: %s", MaskURL(m.url), maskError(err, m.url))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode, strings.TrimSpace(strings.ReplaceAll(string(body), "\n", " ")), nil
}

// MaskURL returns the URL without the secret last path element: the webhook id
// may never end up in a log line or an error message.
func MaskURL(u *url.URL) string {
	if u == nil {
		return "(no url)"
	}
	path := u.Path
	segment, start := lastSegment(path)
	if segment == "" {
		return u.Scheme + "://" + u.Host + "…"
	}
	return u.Scheme + "://" + u.Host + path[:start] + "…" + path[start+len(segment):]
}

// maskError removes the webhook id from an error message of net/http, which
// includes the full URL.
func maskError(err error, u *url.URL) string {
	text := err.Error()
	if u == nil {
		return text
	}
	if id, _ := lastSegment(u.Path); id != "" {
		text = strings.ReplaceAll(text, id, "…")
	}
	return text
}

// lastSegment returns the last *non-empty* path segment and where it starts.
// That "non-empty" is the whole reason this function exists: if you cut at the
// last /, then for a path ending in / the last segment is empty and the webhook
// id stays in full — exactly the opposite of what was intended. checkWebhook
// trims a trailing /, but this function must also be right if a URL ever gets
// past that check.
func lastSegment(path string) (segment string, start int) {
	end := len(path)
	for end > 0 && path[end-1] == '/' {
		end--
	}
	if end == 0 {
		return "", -1
	}
	start = strings.LastIndex(path[:end], "/") + 1
	return path[start:end], start
}

// Examples picks at most five paths for the notification and truncates them.
// Truncating on runes and not on bytes, otherwise a path with accents yields
// half a character and the JSON is fine but the display is not.
//
// cleanText and not only truncating: since the paths are decoded, they may hold
// a control character we no longer want to meet here. For a path that is not
// decodable, Path equals the raw form, and that is exactly what you want to see
// in an example.
func Examples(list []DeletedFile) []string {
	out := make([]string, 0, maxExamples)
	for i, v := range list {
		if i >= maxExamples {
			break
		}
		out = append(out, cleanText(v.Path, maxExampleLength))
	}
	return out
}

// Folders returns the three directories with the most deletions, relative to
// the mountpoint. That is the information that answers "was this an accident?"
// on a phone screen, without a single photo having to leave the house.
func Folders(list []DeletedFile, mountpoint string, max int) []string {
	count := map[string]int{}
	for _, v := range list {
		dir := filepath.Dir(v.Path)
		if mountpoint != "" {
			if rel, ok := relativeWithin(mountpoint, dir); ok {
				dir = rel
			}
		}
		if dir == "" || dir == "." {
			dir = "(root)"
		}
		count[cleanText(dir, maxFolderNameLength)]++
	}
	names := make([]string, 0, len(count))
	for name := range count {
		names = append(names, name)
	}
	// Descending by count, by name when counts are equal: otherwise the
	// notification shows a different order every run.
	sort.Slice(names, func(i, j int) bool {
		if count[names[i]] == count[names[j]] {
			return names[i] < names[j]
		}
		return count[names[i]] > count[names[j]]
	})
	if len(names) > max {
		names = names[:max]
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("%s (%d)", name, count[name]))
	}
	return out
}

// jsonShort makes the payload readable for one log line during a dry run. The
// examples are already truncated in it and the webhook id is not in the
// payload, so this is safe to log.
func jsonShort(p Payload) (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("payload cannot be encoded as JSON: %w", err)
	}
	return string(data), nil
}
