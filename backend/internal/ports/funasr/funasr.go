// Package funasr defines the application-facing Fun-ASR ports.
package funasr

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const DefaultModel = "fun-asr"

var (
	ErrNotConfigured = errors.New("fun-asr not configured")
	ErrEmptyResult   = errors.New("fun-asr empty result")
)

type SubmitInput struct {
	FileURL string
}

type SubmitResult struct {
	TaskID    string
	RequestID string
	Raw       json.RawMessage
}

type TaskStatus struct {
	TaskID           string
	Status           string
	TranscriptionURL string
	Message          string
	Raw              json.RawMessage
	Terminal         bool
	Succeeded        bool
}

type Segment struct {
	SegmentID     string  `json:"segmentID"`
	StartMs       int64   `json:"startMs"`
	EndMs         int64   `json:"endMs"`
	SpeakerID     *int    `json:"speakerID"`
	Text          string  `json:"text"`
	OriginalText  string  `json:"originalText"`
	AvgConfidence float64 `json:"avgConfidence,omitempty"`
	LowConfidence bool    `json:"lowConfidence,omitempty"`
	Edited        bool    `json:"edited,omitempty"`
}

type TranscriptDocument struct {
	Version          int               `json:"version"`
	Revision         int               `json:"revision"`
	Source           string            `json:"source"`
	Model            string            `json:"model"`
	FileID           string            `json:"fileID,omitempty"`
	FileName         string            `json:"fileName,omitempty"`
	DurationMs       *int64            `json:"durationMs,omitempty"`
	SpeakerNames     map[string]string `json:"speakerNames"`
	SpeakerOverrides map[string]string `json:"speakerOverrides,omitempty"`
	Segments         []Segment         `json:"segments"`
	UpdatedAt        string            `json:"updatedAt,omitempty"`
}

type Client interface {
	Submit(context.Context, SubmitInput) (*SubmitResult, error)
	GetTask(context.Context, string) (*TaskStatus, error)
	DownloadJSON(context.Context, string) (json.RawMessage, error)
	Configured() bool
	Model() string
}

type Codec interface {
	Normalize(json.RawMessage, string) (*TranscriptDocument, error)
	RenderMarkdown(*TranscriptDocument, bool) string
	BuildTimeWindowChunks(*TranscriptDocument, time.Duration, time.Duration) []string
}
