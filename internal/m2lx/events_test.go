package m2lx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// liveOverviewBody is the VERBATIM body captured from
// GET /api/events/overview on the live instance on 2026-08-12, trimmed to the
// fields ListEvents reads plus enough of the rest to prove the extra fields are
// ignored rather than tripping the decode. One event, "MatchT", Running.
const liveOverviewBody = `[
  {
    "status": "Running",
    "event_name": "MatchT",
    "event_id": "dl9-5p5ah0bd-empd",
    "endpoints": [
      {"name":"avrouter","url":"m2lx-wslstudios-matcht.etapsiota.com:8001"},
      {"name":"switcher","url":"m2lx-wslstudios-matcht.etapsiota.com:443"}
    ],
    "event_type": "type_A",
    "region": "ap-northeast-1",
    "user_id": "1002",
    "is_invalid_audio_mixer_mode": false
  }
]`

func TestClient_ListEvents_RequiresSignIn(t *testing.T) {
	c := newClient("example.com")
	defer c.Close()
	if _, err := c.ListEvents(context.Background()); err != ErrNotSignedIn {
		t.Fatalf("ListEvents before sign-in: err = %v, want ErrNotSignedIn", err)
	}
}

func TestClient_ListEvents_MeasuredShape(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != eventsOverviewPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, eventsOverviewPath)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("Authorization = %q, want Bearer tok-1", got)
		}
		// The live call sends no query string and no body; assert we send none.
		if r.URL.RawQuery != "" {
			t.Fatalf("unexpected query string %q — the overview endpoint takes no params", r.URL.RawQuery)
		}
		w.Write([]byte(liveOverviewBody))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	events, err := c.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.ID != "dl9-5p5ah0bd-empd" {
		t.Fatalf("ID = %q, want the wire event_id", got.ID)
	}
	if got.Name != "MatchT" {
		t.Fatalf("Name = %q, want the wire event_name", got.Name)
	}
	if got.Status != "Running" {
		t.Fatalf("Status = %q, want Running", got.Status)
	}
}

func TestClient_ListEvents_SortsByNameAndDropsIdless(t *testing.T) {
	// A multi-event instance — the case that turns into a picker. Deliberately
	// out of order, mixed case, and with one entry that has no id (which must
	// be dropped, not offered, because it cannot feed the KVS calls).
	body := `[
	  {"event_id":"z-zulu-1","event_name":"Zulu","status":"Stopped"},
	  {"event_id":"","event_name":"Ghost","status":"Running"},
	  {"event_id":"a-alpha-1","event_name":"alpha","status":"Running"},
	  {"event_id":"m-mike-1","event_name":"Mike","status":"Starting"}
	]`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	events, err := c.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	// alpha, Mike, Zulu — case-insensitive by name; Ghost dropped for no id.
	want := []string{"a-alpha-1", "m-mike-1", "z-zulu-1"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v (the id-less entry must be dropped)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestClient_ListEvents_EmptyInstanceIsNotAnError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newClient(hostOf(t, srv.URL))
	c.httpClient = srv.Client()
	c.token = "tok-1"
	defer c.Close()

	events, err := c.ListEvents(context.Background())
	if err != nil {
		t.Fatalf("empty instance should not error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0", len(events))
	}
}
