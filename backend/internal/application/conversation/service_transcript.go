package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/pkg/textutil"
	portfunasr "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/funasr"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/shared/background"
)

const (
	transcriptCandidateCleanupTimeout = 10 * time.Second
	transcriptPreviewMaxRunes         = 280
)

type TranscriptResult struct {
	FileID       string
	Document     portfunasr.TranscriptDocument
	Markdown     string
	RawPath      string
	JSONPath     string
	MarkdownPath string
}

type TranscriptPatch struct {
	Revision         int
	SpeakerNames     map[string]string
	SpeakerOverrides map[string]string
	Segments         []TranscriptSegmentPatch
}

type TranscriptSegmentPatch struct {
	SegmentID string
	Text      string
}

func (s *Service) GetFileTranscript(ctx context.Context, userID uint, fileID string) (*TranscriptResult, error) {
	fileObj, err := s.repo.GetActiveFileObjectByID(ctx, userID, strings.TrimSpace(fileID))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrFileNotFound) {
			return nil, ErrFileNotFound
		}
		return nil, err
	}
	if fileObj == nil {
		return nil, ErrFileNotFound
	}
	if fileObj.FileCategory != "audio" || fileObj.ExtractStatus != "ready" {
		return nil, ErrFileProcessingNotReady
	}
	processing, err := s.repo.GetFileObjectProcessingByObjectID(ctx, fileObj.ID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrFileNotFound) {
			return nil, ErrFileProcessingNotReady
		}
		return nil, err
	}
	if processing == nil {
		return nil, ErrFileProcessingNotReady
	}
	var payload processingTranscriptPayload
	if err = json.Unmarshal([]byte(processing.PayloadJSON), &payload); err != nil || payload.TranscriptJSONPath == "" {
		return nil, ErrFileProcessingNotReady
	}
	store, err := s.storeProvider.Open(ctx)
	if err != nil {
		return nil, err
	}
	jsonData, err := readTranscriptObject(ctx, store, payload.TranscriptJSONPath)
	if err != nil {
		return nil, err
	}
	var doc portfunasr.TranscriptDocument
	if err = json.Unmarshal(jsonData, &doc); err != nil {
		return nil, err
	}
	return &TranscriptResult{
		FileID:       fileObj.FileID,
		Document:     doc,
		JSONPath:     payload.TranscriptJSONPath,
		MarkdownPath: payload.TranscriptMDPath,
		RawPath:      payload.RawResultPath,
	}, nil
}

func (s *Service) PatchFileTranscript(ctx context.Context, userID uint, fileID string, patch TranscriptPatch) (*TranscriptResult, error) {
	normalizedFileID := strings.TrimSpace(fileID)
	result, err := s.GetFileTranscript(ctx, userID, normalizedFileID)
	if err != nil {
		return nil, err
	}
	if patch.Revision != result.Document.Revision {
		return nil, ErrTranscriptRevisionConflict
	}

	result.Document.Revision++
	result.Document.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	for key, name := range patch.SpeakerNames {
		key = strings.TrimSpace(key)
		if key == "" || strings.TrimSpace(name) == "" || len([]rune(name)) > 80 {
			return nil, ErrTranscriptInvalidEdit
		}
		if result.Document.SpeakerNames == nil {
			result.Document.SpeakerNames = map[string]string{}
		}
		if _, exists := result.Document.SpeakerNames[key]; !exists {
			return nil, ErrTranscriptInvalidEdit
		}
		result.Document.SpeakerNames[key] = strings.TrimSpace(name)
	}
	segments := make(map[string]*portfunasr.Segment, len(result.Document.Segments))
	for i := range result.Document.Segments {
		segments[result.Document.Segments[i].SegmentID] = &result.Document.Segments[i]
	}
	for _, segmentPatch := range patch.Segments {
		segmentID := strings.TrimSpace(segmentPatch.SegmentID)
		segment := segments[segmentID]
		text := strings.TrimSpace(segmentPatch.Text)
		if segment == nil || text == "" || len([]rune(text)) > 20000 {
			return nil, ErrTranscriptInvalidEdit
		}
		segment.Text = text
		segment.Edited = true
	}
	for segmentID, speakerKey := range patch.SpeakerOverrides {
		segmentID = strings.TrimSpace(segmentID)
		segment := segments[segmentID]
		if segment == nil {
			return nil, ErrTranscriptInvalidEdit
		}
		speakerKey = strings.TrimSpace(speakerKey)
		if speakerKey == "" {
			if result.Document.SpeakerOverrides != nil {
				delete(result.Document.SpeakerOverrides, segmentID)
			}
			continue
		}
		if _, exists := result.Document.SpeakerNames[speakerKey]; !exists {
			return nil, ErrTranscriptInvalidEdit
		}
		rawSpeakerKey := ""
		if segment.SpeakerID != nil {
			rawSpeakerKey = fmt.Sprintf("%d", *segment.SpeakerID)
		}
		if speakerKey == rawSpeakerKey {
			if result.Document.SpeakerOverrides != nil {
				delete(result.Document.SpeakerOverrides, segmentID)
			}
			continue
		}
		if result.Document.SpeakerOverrides == nil {
			result.Document.SpeakerOverrides = make(map[string]string)
		}
		result.Document.SpeakerOverrides[segmentID] = speakerKey
	}
	if len(result.Document.SpeakerOverrides) == 0 {
		result.Document.SpeakerOverrides = nil
	}

	if s.funASRCodec == nil {
		return nil, ErrTranscriptInvalidEdit
	}
	result.Markdown = s.funASRCodec.RenderMarkdown(&result.Document, true)
	jsonData, err := json.MarshalIndent(result.Document, "", "  ")
	if err != nil {
		return nil, err
	}
	store, err := s.storeProvider.Open(ctx)
	if err != nil {
		return nil, err
	}

	nextRevision := result.Document.Revision
	candidateRoot := filepath.Join(
		".transcripts",
		fmt.Sprintf("uid_%d", userID),
		normalizedFileID,
		"revisions",
		fmt.Sprintf("rev-%d-%s", nextRevision, uuid.NewString()),
	)
	candidateJSONPath := filepath.ToSlash(filepath.Join(candidateRoot, "transcript.json"))
	candidateMarkdownPath := filepath.ToSlash(filepath.Join(candidateRoot, "transcript.md"))
	result.JSONPath = candidateJSONPath
	result.MarkdownPath = candidateMarkdownPath

	if err = putTranscriptObject(ctx, store, candidateJSONPath, jsonData, "application/json"); err != nil {
		return nil, transcriptWriteFailure(err, ctx, store, candidateJSONPath, candidateMarkdownPath)
	}
	if err = putTranscriptObject(ctx, store, candidateMarkdownPath, []byte(result.Markdown), "text/markdown; charset=utf-8"); err != nil {
		return nil, transcriptWriteFailure(err, ctx, store, candidateJSONPath, candidateMarkdownPath)
	}

	published, err := s.repo.PublishTranscriptRevision(ctx, userID, normalizedFileID, repository.PublishTranscriptRevisionInput{
		ExpectedRevision:   patch.Revision,
		Revision:           nextRevision,
		TranscriptJSONPath: candidateJSONPath,
		TranscriptMDPath:   candidateMarkdownPath,
		ExtractChars:       len([]rune(result.Markdown)),
		PreviewText:        textutil.CompactSnippet(result.Markdown, transcriptPreviewMaxRunes),
	})
	if err != nil {
		return nil, transcriptWriteFailure(err, ctx, store, candidateJSONPath, candidateMarkdownPath)
	}
	if !published {
		cleanupErr := cleanupTranscriptCandidates(ctx, store, candidateJSONPath, candidateMarkdownPath)
		if cleanupErr != nil {
			return nil, errors.Join(ErrTranscriptRevisionConflict, cleanupErr)
		}
		return nil, ErrTranscriptRevisionConflict
	}

	fileObj, lookupErr := s.repo.GetActiveFileObjectByID(ctx, userID, normalizedFileID)
	if lookupErr == nil && fileObj != nil && s.embeddingSvc != nil && s.embeddingSvc.ShouldTrigger(*fileObj) {
		fileObj.ExtractStoragePath = result.MarkdownPath
		fileObj.ExtractStatus = "ready"
		s.embeddingSvc.MaybeTrigger(ctx, *fileObj)
	}
	return result, nil
}

type processingTranscriptPayload struct {
	RawResultPath      string `json:"rawResultPath"`
	TranscriptJSONPath string `json:"transcriptJSONPath"`
	TranscriptMDPath   string `json:"transcriptMDPath"`
	TranscriptRevision int    `json:"transcriptRevision"`
}

func transcriptWriteFailure(cause error, ctx context.Context, store objectstore.Store, paths ...string) error {
	cleanupErr := cleanupTranscriptCandidates(ctx, store, paths...)
	if cleanupErr == nil {
		return cause
	}
	return errors.Join(cause, cleanupErr)
}

func cleanupTranscriptCandidates(ctx context.Context, store objectstore.Store, paths ...string) error {
	if store == nil {
		return nil
	}
	cleanupCtx, cancel := background.WithTimeout(ctx, transcriptCandidateCleanupTimeout)
	defer cancel()
	var cleanupErrs []error
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := store.Delete(cleanupCtx, path); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete transcript candidate: %w", err))
		}
	}
	return errors.Join(cleanupErrs...)
}

func readTranscriptObject(ctx context.Context, store objectstore.Store, path string) ([]byte, error) {
	reader, _, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	return data, errors.Join(readErr, closeErr)
}

func putTranscriptObject(ctx context.Context, store objectstore.Store, path string, data []byte, contentType string) error {
	_, err := store.Put(ctx, path, bytes.NewReader(data), appstoragePutOptions(contentType, int64(len(data))))
	return err
}

func appstoragePutOptions(contentType string, size int64) objectstore.PutOptions {
	return objectstore.PutOptions{ContentType: contentType, SizeBytes: size}
}
