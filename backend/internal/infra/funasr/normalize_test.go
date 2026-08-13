package funasr

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRawAndMarkdown(t *testing.T) {
	raw := json.RawMessage(`{
	  "properties": {"original_duration_in_milliseconds": 366000},
	  "transcripts": [{
	    "sentences": [
	      {"sentence_id": 1, "begin_time": 0, "end_time": 1000, "speaker_id": 0, "text": "你好", "words": [{"text":"你好","confidence":0.9}]},
	      {"sentence_id": 2, "begin_time": 120000, "end_time": 121000, "speaker_id": 1, "text": "你好啊", "words": [{"text":"你好啊","confidence":0.8}]},
	      {"sentence_id": 3, "begin_time": 240000, "end_time": 241000, "speaker_id": 0, "text": "再见", "words": [{"text":"再见","confidence":0.7}]}
	    ]
	  }]
	}`)
	doc, err := NormalizeRaw(raw, "fun-asr")
	if err != nil {
		t.Fatalf("NormalizeRaw: %v", err)
	}
	if len(doc.Segments) != 3 {
		t.Fatalf("segments=%d", len(doc.Segments))
	}
	if doc.SpeakerNames["0"] == "" || doc.SpeakerNames["1"] == "" {
		t.Fatalf("speaker names missing: %#v", doc.SpeakerNames)
	}
	md := RenderMarkdown(doc, false)
	if !strings.Contains(md, "说话人1") || !strings.Contains(md, "你好") {
		t.Fatalf("markdown unexpected: %s", md)
	}
	chunks := BuildTimeWindowChunks(doc, 2*time.Minute, 15*time.Second)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple time windows, got %d: %#v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "[") || !strings.Contains(chunks[0], "说话人") {
		t.Fatalf("chunk missing header/speaker: %s", chunks[0])
	}
}

func TestLowConfidenceFlag(t *testing.T) {
	raw := json.RawMessage(`{
	  "transcripts": [{
	    "sentences": [{
	      "sentence_id": 1,
	      "begin_time": 0,
	      "end_time": 1000,
	      "speaker_id": 0,
	      "text": "Marikit",
	      "words": [
	        {"text":"Marikit","confidence":0.2},
	        {"text":"pa","confidence":0.01}
	      ]
	    }]
	  }]
	}`)
	doc, err := NormalizeRaw(raw, "fun-asr")
	if err != nil {
		t.Fatalf("NormalizeRaw: %v", err)
	}
	if !doc.Segments[0].LowConfidence {
		t.Fatal("expected low confidence")
	}
}
