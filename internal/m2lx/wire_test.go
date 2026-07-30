package m2lx

import "testing"

// A realistic switcher_status push, built from the measured shapes in
// docs/architecture.md (line 406: "h264 1920x1080 50 P", "aac 48000 2ch")
// and the WS node names in docs/architecture.md §3.2 ("cam1"-"cam24").
const samplePayload = `{
  "cam7": {
    "stream_state": "streaming",
    "streams": {
      "video": {"format": "h264 1920x1080 50 P"},
      "audio": [{"format": "aac 48000 2ch"}]
    }
  },
  "cam8": {
    "stream_state": "stopped",
    "streams": {
      "video": {"format": "h264 1920x1080 50 P"},
      "audio": []
    }
  },
  "MIC 1": {"stream_state": "streaming"}
}`

func TestExtractNode_KeyPresent(t *testing.T) {
	node, ok, err := extractNode([]byte(samplePayload), "cam7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for a present key")
	}
	if node.StreamState != "streaming" {
		t.Fatalf("StreamState = %q, want streaming", node.StreamState)
	}
	if node.Streams.Video.Format != "h264 1920x1080 50 P" {
		t.Fatalf("Video.Format = %q", node.Streams.Video.Format)
	}
	if len(node.Streams.Audio) != 1 || node.Streams.Audio[0].Format != "aac 48000 2ch" {
		t.Fatalf("Audio = %+v", node.Streams.Audio)
	}
}

// This is the MP2/AC-3 silent-drop signature (windows-app-spec.md §8): the
// audio array is present but empty. It must extract cleanly, not error.
func TestExtractNode_EmptyAudioArraySurvives(t *testing.T) {
	node, ok, err := extractNode([]byte(samplePayload), "cam8")
	if err != nil || !ok {
		t.Fatalf("extractNode(cam8) = (ok=%v, err=%v)", ok, err)
	}
	if node.Streams.Audio == nil {
		t.Fatalf("Streams.Audio is nil, want a non-nil empty slice from an explicit []")
	}
	if len(node.Streams.Audio) != 0 {
		t.Fatalf("Streams.Audio = %+v, want empty", node.Streams.Audio)
	}
}

// A node that only reports stream_state (no streams object at all — e.g. a
// non-router node like a MIC) must not error; the zero-value Streams is
// exactly what an absent field decodes to.
func TestExtractNode_NodeWithoutStreams(t *testing.T) {
	node, ok, err := extractNode([]byte(samplePayload), "MIC 1")
	if err != nil || !ok {
		t.Fatalf("extractNode(MIC 1) = (ok=%v, err=%v)", ok, err)
	}
	if node.StreamState != "streaming" {
		t.Fatalf("StreamState = %q", node.StreamState)
	}
	if len(node.Streams.Audio) != 0 || node.Streams.Video.Format != "" {
		t.Fatalf("expected zero-value Streams, got %+v", node.Streams)
	}
}

func TestExtractNode_KeyAbsent(t *testing.T) {
	node, ok, err := extractNode([]byte(samplePayload), "cam99")
	if err != nil {
		t.Fatalf("a message that simply omits our key must not be an error, got %v", err)
	}
	if ok {
		t.Fatalf("ok = true for an absent key")
	}
	if node.StreamState != "" || node.Streams.Video.Format != "" || len(node.Streams.Audio) != 0 {
		t.Fatalf("node = %+v, want zero value when not found", node)
	}
}

func TestExtractNode_MalformedTopLevel(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `not json at all`},
		{"top level array", `["cam7", "cam8"]`},
		{"top level string", `"streaming"`},
		{"empty body", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := extractNode([]byte(tc.body), "cam7")
			if err == nil {
				t.Fatalf("expected an error for malformed top-level payload %q", tc.body)
			}
			if ok {
				t.Fatalf("ok = true alongside an error")
			}
		})
	}
}

func TestExtractNode_NodeWithUnexpectedShapeErrorsInsteadOfPanicking(t *testing.T) {
	// stream_state is a number instead of a string: must be reported as an
	// error naming the node, never a panic.
	payload := `{"cam7": {"stream_state": 12345}}`
	_, ok, err := extractNode([]byte(payload), "cam7")
	if err == nil {
		t.Fatalf("expected an error for a node whose stream_state is not a string")
	}
	if ok {
		t.Fatalf("ok = true alongside an error")
	}
}

func TestExtractNode_VideoFormatWrongTypeErrorsInsteadOfPanicking(t *testing.T) {
	payload := `{"cam7": {"stream_state": "streaming", "streams": {"video": "not an object"}}}`
	_, ok, err := extractNode([]byte(payload), "cam7")
	if err == nil {
		t.Fatalf("expected an error when streams.video is not an object")
	}
	if ok {
		t.Fatalf("ok = true alongside an error")
	}
}

func TestExtractNode_AudioArrayWithExtraUnknownFields(t *testing.T) {
	// The wire node may carry fields WP-2 doesn't model (e.g. bitrate,
	// packet counters). Unknown fields must be ignored, not rejected.
	payload := `{
	  "cam7": {
	    "stream_state": "streaming",
	    "statistics": {"bitrate": 4300000},
	    "streams": {
	      "video": {"format": "h264 1920x1080 50 P", "width": 1920, "height": 1080},
	      "audio": [
	        {"format": "aac 48000 2ch", "lang": "eng"},
	        {"format": "aac 48000 2ch", "lang": "fra"}
	      ]
	    }
	  }
	}`
	node, ok, err := extractNode([]byte(payload), "cam7")
	if err != nil || !ok {
		t.Fatalf("extractNode = (ok=%v, err=%v)", ok, err)
	}
	if len(node.Streams.Audio) != 2 {
		t.Fatalf("Audio = %+v, want 2 entries", node.Streams.Audio)
	}
}
