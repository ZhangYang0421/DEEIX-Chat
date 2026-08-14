package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/funasr"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
)

type TranscriptResult struct {
	FileID       string
	Document     funasr.TranscriptDocument
	Markdown     string
	RawPath      string
	JSONPath     string
	MarkdownPath string
}

type TranscriptPatch struct {
	Revision     int
	SpeakerNames map[string]string
	Segments     []TranscriptSegmentPatch
}

type TranscriptSegmentPatch struct {
	SegmentID string
	Text      string
}

func (s *Service) GetFileTranscript(ctx context.Context, userID uint, fileID string) (*TranscriptResult, error) {
	fileObj, err := s.repo.GetActiveFileObjectByID(ctx, userID, strings.TrimSpace(fileID))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrFileNotFound
		}
		return nil, err
	}
	if fileObj == nil || fileObj.FileCategory != "audio" || fileObj.ExtractStatus != "ready" {
		return nil, ErrFileProcessingNotReady
	}
	processing, err := s.repo.GetFileObjectProcessingByObjectID(ctx, fileObj.ID)
	if err != nil || processing == nil {
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
	reader, _, err := store.Open(ctx, payload.TranscriptJSONPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var doc funasr.TranscriptDocument
	if err = json.NewDecoder(reader).Decode(&doc); err != nil {
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
	lockedRevision, err := s.repo.CompareAndSwapTranscriptRevision(ctx, userID, normalizedFileID, patch.Revision)
	if err != nil {
		return nil, err
	}
	if !lockedRevision {
		return nil, ErrTranscriptRevisionConflict
	}
	persisted := false
	defer func() {
		if !persisted {
			_, _ = s.repo.CompareAndSwapTranscriptRevision(context.Background(), userID, normalizedFileID, patch.Revision+1)
		}
	}()

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
	segments := make(map[string]*funasr.Segment, len(result.Document.Segments))
	for i := range result.Document.Segments {
		segments[result.Document.Segments[i].SegmentID] = &result.Document.Segments[i]
	}
	for _, change := range patch.Segments {
		segment := segments[strings.TrimSpace(change.SegmentID)]
		if segment == nil || strings.TrimSpace(change.Text) == "" || len([]rune(change.Text)) > 20000 {
			return nil, ErrTranscriptInvalidEdit
		}
		segment.Text = strings.TrimSpace(change.Text)
		segment.Edited = segment.Text != segment.OriginalText
	}

	result.Markdown = funasr.RenderMarkdown(&result.Document, true)
	jsonData, err := json.MarshalIndent(result.Document, "", "  ")
	if err != nil {
		return nil, err
	}
	store, err := s.storeProvider.Open(ctx)
	if err != nil {
		return nil, err
	}
	jsonTempPath := result.JSONPath + fmt.Sprintf(".rev-%d.tmp", result.Document.Revision)
	markdownTempPath := result.MarkdownPath + fmt.Sprintf(".rev-%d.tmp", result.Document.Revision)
	if _, err = store.Put(ctx, jsonTempPath, strings.NewReader(string(jsonData)), appstoragePutOptions("application/json", int64(len(jsonData)))); err != nil {
		return nil, err
	}
	defer func() { _ = store.Delete(context.Background(), jsonTempPath) }()
	if _, err = store.Put(ctx, markdownTempPath, strings.NewReader(result.Markdown), appstoragePutOptions("text/markdown; charset=utf-8", int64(len(result.Markdown)))); err != nil {
		return nil, err
	}
	defer func() { _ = store.Delete(context.Background(), markdownTempPath) }()
	if err = copyTranscriptObject(ctx, store, jsonTempPath, result.JSONPath, "application/json"); err != nil {
		return nil, err
	}
	if err = copyTranscriptObject(ctx, store, markdownTempPath, result.MarkdownPath, "text/markdown; charset=utf-8"); err != nil {
		return nil, err
	}
	persisted = true

	fileObj, lookupErr := s.repo.GetActiveFileObjectByID(ctx, userID, normalizedFileID)
	if lookupErr == nil && fileObj != nil && s.embeddingSvc != nil && s.embeddingSvc.ShouldTrigger(*fileObj) {
		fileObj.ExtractStoragePath = result.MarkdownPath
		fileObj.ExtractStatus = "ready"
		s.embeddingSvc.Trigger(*fileObj)
	}
	return result, nil
}

type processingTranscriptPayload struct {
	RawResultPath      string `json:"rawResultPath"`
	TranscriptJSONPath string `json:"transcriptJSONPath"`
	TranscriptMDPath   string `json:"transcriptMDPath"`
}

func copyTranscriptObject(ctx context.Context, store objectstore.Store, sourcePath string, targetPath string, contentType string) error {
	reader, info, err := store.Open(ctx, sourcePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = store.Put(ctx, targetPath, reader, objectstore.PutOptions{ContentType: contentType, SizeBytes: info.SizeBytes})
	return err
}

func appstoragePutOptions(contentType string, size int64) objectstore.PutOptions {
	return objectstore.PutOptions{ContentType: contentType, SizeBytes: size}
}
