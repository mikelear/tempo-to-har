// Package synth converts Tempo span trees into HAR (HTTP Archive) entries.
//
// Tempo returns OTLP-formatted span data. We parse out the HTTP-flavored
// spans (those carrying http.method / http.url / http.status_code attrs)
// and map each into an HAR entry. The result conforms to HAR 1.2:
// http://www.softwareishard.com/blog/har-12-spec/
//
// What we capture:
//   - URL, method, status (from span attributes)
//   - timing (from start/end span timestamps)
//   - cluster + service name (in the HAR creator block + per-entry comment)
//
// What we DON'T have from Tempo by default:
//   - request body (not captured by OTel SDK by default)
//   - response body (same)
//   - full request/response headers (only what's in span attributes)
//
// That's fine — load-testing replay (Phase 2.2) gets URL+method+status;
// traffic-forensics (Phase 2.7) gets edges+rates+latencies. Body content
// is NOT required for either consumer in their initial form.
package synth

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// otlpExportTrace is the top-level shape Tempo returns from /api/traces/<id>.
// We only model the fields we actually read.
type otlpExportTrace struct {
	Batches []resourceSpans `json:"batches"`
}

type resourceSpans struct {
	Resource                    resourceObj `json:"resource"`
	InstrumentationLibrarySpans []ilSpans   `json:"instrumentationLibrarySpans"`
	ScopeSpans                  []ilSpans   `json:"scopeSpans"` // newer OTLP variant
}

type resourceObj struct {
	Attributes []kv `json:"attributes"`
}

type ilSpans struct {
	Spans []span `json:"spans"`
}

type span struct {
	Name              string `json:"name"`
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	EndTimeUnixNano   string `json:"endTimeUnixNano"`
	Attributes        []kv   `json:"attributes"`
	Kind              string `json:"kind"`
}

type kv struct {
	Key   string  `json:"key"`
	Value kvValue `json:"value"`
}

type kvValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
}

func (v kvValue) String() string {
	if v.StringValue != nil {
		return *v.StringValue
	}
	if v.IntValue != nil {
		return *v.IntValue
	}
	if v.BoolValue != nil {
		if *v.BoolValue {
			return "true"
		}
		return "false"
	}
	return ""
}

// HAR is the top-level HAR 1.2 envelope.
type HAR struct {
	Log HARLog `json:"log"`
}

// HARLog is the page-level container in a HAR 1.2 envelope.
type HARLog struct {
	Version string     `json:"version"`
	Creator HARCreator `json:"creator"`
	Entries []HAREntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

// HARCreator identifies the tool that produced the HAR (per HAR 1.2 spec).
type HARCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Comment string `json:"comment,omitempty"`
}

// HAREntry is one request/response pair in the HAR log.
type HAREntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"` // ms
	Request         HARRequest  `json:"request"`
	Response        HARResponse `json:"response"`
	Cache           HARCache    `json:"cache"`
	Timings         HARTimings  `json:"timings"`
	Comment         string      `json:"comment,omitempty"`
}

// HARRequest is the request portion of a HAR entry.
type HARRequest struct {
	Method      string          `json:"method"`
	URL         string          `json:"url"`
	HTTPVersion string          `json:"httpVersion"`
	Headers     []HARHeader     `json:"headers"`
	QueryString []HARQueryParam `json:"queryString"`
	Cookies     []HARCookie     `json:"cookies"`
	HeadersSize int             `json:"headersSize"`
	BodySize    int             `json:"bodySize"`
}

// HARResponse is the response portion of a HAR entry.
type HARResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []HARHeader `json:"headers"`
	Cookies     []HARCookie `json:"cookies"`
	Content     HARContent  `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

// HARHeader is a single name/value HTTP header.
type HARHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HARQueryParam is a single name/value URL query parameter.
type HARQueryParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HARCookie is a single name/value cookie.
type HARCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HARContent describes the body of a HAR response.
type HARContent struct {
	Size     int    `json:"size"`
	MimeType string `json:"mimeType"`
}

// HARCache is a placeholder per HAR 1.2 — not used here.
type HARCache struct{}

// HARTimings breaks down a HAR entry's request lifecycle.
type HARTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// FromTempo takes the raw OTLP JSON Tempo returned + metadata, and returns
// a HAR with one entry per HTTP-flavored span.
func FromTempo(tempoBody []byte, serviceName, clusterTag, sourceTag string) (*HAR, error) {
	var trace otlpExportTrace
	if err := json.Unmarshal(tempoBody, &trace); err != nil {
		return nil, fmt.Errorf("decode OTLP: %w", err)
	}

	entries := []HAREntry{}
	for _, batch := range trace.Batches {
		// Newer OTLP uses scopeSpans; older uses instrumentationLibrarySpans.
		// Concat both — only one will be populated per batch.
		ils := append([]ilSpans{}, batch.InstrumentationLibrarySpans...)
		ils = append(ils, batch.ScopeSpans...)

		for _, il := range ils {
			for _, sp := range il.Spans {
				entry, ok := spanToHAREntry(sp)
				if !ok {
					continue // not an HTTP span
				}
				entries = append(entries, entry)
			}
		}
	}

	creator := HARCreator{
		Name:    "tempo-to-har",
		Version: "v1",
		Comment: fmt.Sprintf("synthesized from Tempo for service=%s cluster=%s source=%s", serviceName, clusterTag, sourceTag),
	}

	return &HAR{
		Log: HARLog{
			Version: "1.2",
			Creator: creator,
			Entries: entries,
		},
	}, nil
}

// spanToHAREntry returns ok=false if the span has no HTTP attributes.
func spanToHAREntry(sp span) (HAREntry, bool) {
	attrs := map[string]string{}
	for _, a := range sp.Attributes {
		attrs[a.Key] = a.Value.String()
	}

	method, hasMethod := attrs["http.method"]
	if !hasMethod {
		method, hasMethod = attrs["http.request.method"] // newer semantic-conventions key
	}
	if !hasMethod {
		return HAREntry{}, false
	}

	url := firstNonEmpty(attrs["http.url"], attrs["url.full"], attrs["http.target"])
	if url == "" {
		return HAREntry{}, false
	}

	statusStr := firstNonEmpty(attrs["http.status_code"], attrs["http.response.status_code"])
	status, _ := strconv.Atoi(statusStr)

	startedAt, _ := nanosToTime(sp.StartTimeUnixNano)
	endedAt, _ := nanosToTime(sp.EndTimeUnixNano)
	durationMs := float64(endedAt.Sub(startedAt).Microseconds()) / 1000.0

	return HAREntry{
		StartedDateTime: startedAt.UTC().Format(time.RFC3339Nano),
		Time:            durationMs,
		Request: HARRequest{
			Method:      method,
			URL:         url,
			HTTPVersion: "HTTP/1.1",
			Headers:     []HARHeader{},
			QueryString: []HARQueryParam{},
			Cookies:     []HARCookie{},
			HeadersSize: -1,
			BodySize:    -1,
		},
		Response: HARResponse{
			Status:      status,
			StatusText:  "",
			HTTPVersion: "HTTP/1.1",
			Headers:     []HARHeader{},
			Cookies:     []HARCookie{},
			Content:     HARContent{Size: 0, MimeType: ""},
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    -1,
		},
		Cache:   HARCache{},
		Timings: HARTimings{Send: 0, Wait: durationMs, Receive: 0},
		Comment: fmt.Sprintf("span name: %s; kind: %s", sp.Name, sp.Kind),
	}, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func nanosToTime(nanoStr string) (time.Time, error) {
	n, err := strconv.ParseInt(nanoStr, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, n), nil
}
