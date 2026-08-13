package funasr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubmitUsesAsyncDiarizationRequest(t *testing.T) {
	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/audio/asr/transcription" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-DashScope-Async") != "enable" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected headers: %#v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"req-1","output":{"task_id":"task-1"}}`))
	}))
	defer server.Close()

	client := New(Config{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()})
	result, err := client.Submit(context.Background(), SubmitInput{FileURL: "https://storage.example.test/file.mp3?signature=redacted"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.TaskID != "task-1" {
		t.Fatalf("task id = %q", result.TaskID)
	}
	params, _ := captured["parameters"].(map[string]interface{})
	input, _ := captured["input"].(map[string]interface{})
	if input["file_url"] != "https://storage.example.test/file.mp3?signature=redacted" {
		t.Fatalf("file_url = %#v", input["file_url"])
	}
	if enabled, _ := params["diarization_enabled"].(bool); !enabled {
		t.Fatalf("diarization must be enabled: %#v", params)
	}
	if _, exists := params["speaker_count"]; exists {
		t.Fatalf("speaker_count must be omitted for automatic estimation: %#v", params)
	}
}

func TestGetTaskReadsNestedTranscriptionURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
		  "output": {
		    "task_id": "task-1",
		    "task_status": "SUCCEEDED",
		    "results": [{
		      "subtask_status": "SUCCEEDED",
		      "output": {
		        "transcription_url": "https://result.example.test/transcript.json"
		      }
		    }]
		  }
		}`))
	}))
	defer server.Close()

	client := New(Config{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()})
	status, err := client.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !status.Terminal || !status.Succeeded || status.TranscriptionURL != "https://result.example.test/transcript.json" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestSubmitRejectsNonHTTPSFileURL(t *testing.T) {
	client := New(Config{APIKey: "test-key"})
	_, err := client.Submit(context.Background(), SubmitInput{FileURL: "http://storage.example.test/file.mp3"})
	if err == nil || !strings.Contains(err.Error(), "public https") {
		t.Fatalf("Submit error = %v, want public https validation", err)
	}
}
