package processing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/funasr"
	infraobjectstore "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/pkg/textutil"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"go.uber.org/zap"
)

const (
	audioPresignExpiry    = 2 * time.Hour
	audioRequestTimeout   = 2 * time.Minute
	audioPollDelay        = 3 * time.Second
	audioRecoveryBatch    = 1000
	audioProcessingEngine = "fun-asr"
)

// audioProcessingPayload 是 file_objects.processing_payload_json 中的最小可恢复状态。
// 临时 Presigned URL 和供应商结果 URL 不落库，避免泄露签名参数。
type audioProcessingPayload struct {
	Version            int    `json:"version"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	Stage              string `json:"stage"`
	TaskID             string `json:"taskID,omitempty"`
	TaskStatus         string `json:"taskStatus,omitempty"`
	RawResultPath      string `json:"rawResultPath,omitempty"`
	TranscriptJSONPath string `json:"transcriptJSONPath,omitempty"`
	TranscriptMDPath   string `json:"transcriptMDPath,omitempty"`
}

func (s *Service) RetryAudioTranscription(ctx context.Context, userID uint, fileID string) error {
	if s == nil || s.repo == nil || userID == 0 || strings.TrimSpace(fileID) == "" {
		return ErrAudioRetryNotAllowed
	}
	fileObj, err := s.repo.GetActiveFileObjectByID(ctx, userID, strings.TrimSpace(fileID))
	if err != nil || fileObj == nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(fileObj.FileCategory), "audio") || fileObj.ProcessingStatus != "failed" {
		return ErrAudioRetryNotAllowed
	}
	payload := parseAudioProcessingPayload(fileObj.ProcessingPayloadJSON)
	payload.Stage = "uploaded"
	payload.TaskID = ""
	payload.TaskStatus = ""
	payload.RawResultPath = ""
	payload.TranscriptJSONPath = ""
	payload.TranscriptMDPath = ""
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	processingStatus := "queued"
	processingReady := false
	extractStatus := "processing"
	errorCode := ""
	errorMessage := ""
	var startedAt *time.Time
	var completedAt *time.Time
	if err = s.repo.UpdateFileObjectProcessingState(ctx, &domainconversation.FileObjectProcessing{
		FileObjectID:     fileObj.ID,
		UserID:           fileObj.UserID,
		DetectedMIME:     fileObj.DetectedMIME,
		FileCategory:     fileObj.FileCategory,
		ProcessingStatus: "queued",
		ExtractStatus:    "processing",
		ExtractEngine:    audioProcessingEngine,
		RAGReady:         false,
		RAGReason:        "transcription_pending",
		PayloadJSON:      string(payloadJSON),
		StartedAt:        nil,
		CompletedAt:      nil,
	}); err != nil {
		return err
	}
	if err = s.repo.UpdateFileObjectProcessing(ctx, userID, fileObj.FileID, repository.UpdateFileObjectProcessingInput{
		ProcessingStatus:       &processingStatus,
		ProcessingReady:        &processingReady,
		ProcessingErrorCode:    &errorCode,
		ProcessingErrorMessage: &errorMessage,
		ExtractStatus:          &extractStatus,
		ProcessingPayloadJSON:  stringPtr(string(payloadJSON)),
		ProcessingStartedAt:    timePtr(startedAt),
		ProcessingCompletedAt:  timePtr(completedAt),
	}); err != nil {
		return err
	}
	return s.enqueueFileProcessing(ctx, userID, fileObj.FileID, 0, "")
}

func (s *Service) processAudioFile(ctx context.Context, fileObj *domainconversation.FileObject) error {
	if fileObj == nil {
		return nil
	}
	if s.transcriber == nil || !s.transcriber.Configured() {
		return s.failAudio(ctx, fileObj, "transcription_not_configured", "录音转写服务未配置")
	}
	store, err := s.openObjectStore(ctx)
	if err != nil {
		return s.failAudio(ctx, fileObj, "transcription_storage_unavailable", "录音存储服务不可用")
	}

	payload := parseAudioProcessingPayload(fileObj.ProcessingPayloadJSON)
	if payload.TaskID == "" {
		return s.submitAudio(ctx, store, fileObj, payload)
	}
	return s.pollAudio(ctx, store, fileObj, payload)
}

func (s *Service) submitAudio(ctx context.Context, store objectstore.Store, fileObj *domainconversation.FileObject, payload audioProcessingPayload) error {
	presigner, ok := store.(interface {
		PresignGet(context.Context, string, time.Duration) (string, error)
	})
	if !ok {
		return s.failAudio(ctx, fileObj, "transcription_requires_s3", "录音转写需要支持预签名下载的对象存储")
	}
	fileURL, err := presigner.PresignGet(ctx, fileObj.StoragePath, audioPresignExpiry)
	if err != nil {
		code := "transcription_presign_failed"
		message := "无法生成录音临时下载地址"
		if errors.Is(err, infraobjectstore.ErrUnsupported) {
			code = "transcription_requires_s3"
			message = "录音转写需要支持预签名下载的对象存储"
		}
		return s.failAudio(ctx, fileObj, code, message)
	}

	requestCtx, cancel := context.WithTimeout(ctx, audioRequestTimeout)
	defer cancel()
	result, err := s.transcriber.Submit(requestCtx, funasr.SubmitInput{FileURL: fileURL})
	if err != nil {
		return s.failAudio(ctx, fileObj, audioErrorCode(err), audioErrorMessage(err))
	}

	now := time.Now()
	payload.Version = 1
	payload.Provider = "dashscope"
	payload.Model = s.transcriber.Model()
	payload.Stage = "polling"
	payload.TaskID = strings.TrimSpace(result.TaskID)
	payload.TaskStatus = "PENDING"
	if err = s.repo.UpdateFileObjectProcessingState(ctx, &domainconversation.FileObjectProcessing{
		FileObjectID:     fileObj.ID,
		UserID:           fileObj.UserID,
		DetectedMIME:     fileObj.DetectedMIME,
		FileCategory:     fileObj.FileCategory,
		ProcessingStatus: "transcribing",
		ExtractStatus:    "processing",
		ExtractEngine:    audioProcessingEngine,
		RAGReady:         false,
		RAGReason:        "transcription_pending",
		PayloadJSON:      string(mustMarshalAudioPayload(payload)),
		StartedAt:        &now,
	}); err != nil {
		return err
	}
	if err = s.persistAudioProgress(ctx, fileObj, payload, "transcribing", "processing", &now, nil); err != nil {
		return err
	}
	s.scheduleAudioPoll(ctx, fileObj.UserID, fileObj.FileID)
	return nil
}

func (s *Service) pollAudio(ctx context.Context, store objectstore.Store, fileObj *domainconversation.FileObject, payload audioProcessingPayload) error {
	requestCtx, cancel := context.WithTimeout(ctx, audioRequestTimeout)
	status, err := s.transcriber.GetTask(requestCtx, payload.TaskID)
	cancel()
	if err != nil {
		return s.failAudio(ctx, fileObj, audioErrorCode(err), audioErrorMessage(err))
	}

	payload.TaskStatus = strings.TrimSpace(status.Status)
	if !status.Terminal {
		payload.Stage = "polling"
		if err = s.persistAudioProgress(ctx, fileObj, payload, "transcribing", "processing", fileObj.ProcessingStartedAt, nil); err != nil {
			return err
		}
		s.scheduleAudioPoll(ctx, fileObj.UserID, fileObj.FileID)
		return nil
	}
	if !status.Succeeded {
		message := "录音转写失败，请手动重试"
		if strings.TrimSpace(status.Message) != "" {
			message = "录音转写失败，请稍后手动重试"
		}
		return s.failAudio(ctx, fileObj, "transcription_failed", message)
	}

	requestCtx, cancel = context.WithTimeout(ctx, audioRequestTimeout)
	rawResult, err := s.transcriber.DownloadJSON(requestCtx, status.TranscriptionURL)
	cancel()
	if err != nil {
		return s.failAudio(ctx, fileObj, "transcription_result_download_failed", "录音转写结果下载失败，请手动重试")
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		model = s.transcriber.Model()
	}
	doc, err := funasr.NormalizeRaw(rawResult, model)
	if err != nil {
		return s.failAudio(ctx, fileObj, "transcription_result_invalid", "录音转写结果无法解析，请手动重试")
	}
	doc.FileID = fileObj.FileID
	doc.FileName = fileObj.FileName
	transcriptJSON, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return s.failAudio(ctx, fileObj, "transcription_result_invalid", "录音转写结果无法保存，请手动重试")
	}
	transcriptMarkdown := funasr.RenderMarkdown(doc, true)

	basePath := filepath.ToSlash(filepath.Join(".transcripts", fmt.Sprintf("uid_%d", fileObj.UserID), fileObj.FileID))
	payload.RawResultPath = basePath + "/result.raw.json"
	payload.TranscriptJSONPath = basePath + "/transcript.json"
	payload.TranscriptMDPath = basePath + "/transcript.md"
	if err = putAudioArtifact(ctx, store, payload.RawResultPath, rawResult, "application/json"); err != nil {
		return s.failAudio(ctx, fileObj, "transcription_storage_failed", "录音转写结果保存失败，请手动重试")
	}
	if err = putAudioArtifact(ctx, store, payload.TranscriptJSONPath, transcriptJSON, "application/json"); err != nil {
		return s.failAudio(ctx, fileObj, "transcription_storage_failed", "录音转写结果保存失败，请手动重试")
	}
	if err = putAudioArtifact(ctx, store, payload.TranscriptMDPath, []byte(transcriptMarkdown), "text/markdown; charset=utf-8"); err != nil {
		return s.failAudio(ctx, fileObj, "transcription_storage_failed", "录音转写结果保存失败，请手动重试")
	}

	payload.Stage = "completed"
	completedAt := time.Now()
	preview := textutil.CompactSnippet(transcriptMarkdown, defaultProcessingPreview)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err = s.repo.UpdateFileObjectProcessingState(ctx, &domainconversation.FileObjectProcessing{
		FileObjectID:       fileObj.ID,
		UserID:             fileObj.UserID,
		DetectedMIME:       fileObj.DetectedMIME,
		FileCategory:       fileObj.FileCategory,
		ProcessingStatus:   "ready",
		ExtractStatus:      "ready",
		ExtractEngine:      audioProcessingEngine,
		ExtractStoragePath: payload.TranscriptMDPath,
		ExtractChars:       len([]rune(transcriptMarkdown)),
		PreviewText:        preview,
		RAGReady:           false,
		RAGReason:          "embedding_pending",
		ExtractorVersion:   s.version(),
		PayloadJSON:        string(payloadJSON),
		StartedAt:          fileObj.ProcessingStartedAt,
		CompletedAt:        &completedAt,
	}); err != nil {
		return err
	}
	fileObj.ExtractStoragePath = payload.TranscriptMDPath
	fileObj.ExtractStatus = "ready"
	fileObj.ExtractEngine = audioProcessingEngine
	fileObj.ProcessingPayloadJSON = string(payloadJSON)
	fileObj.ProcessingCompletedAt = &completedAt
	fileObj.ProcessingStatus = "ready"
	fileObj.ProcessingReady = true
	processingStatus := "ready"
	processingReady := true
	extractStatus := "ready"
	errorCode := ""
	errorMessage := ""
	extractedAt := &completedAt
	if err = s.repo.UpdateFileObjectProcessing(ctx, fileObj.UserID, fileObj.FileID, repository.UpdateFileObjectProcessingInput{
		ProcessingStatus:       &processingStatus,
		ProcessingReady:        &processingReady,
		ProcessingErrorCode:    &errorCode,
		ProcessingErrorMessage: &errorMessage,
		ExtractStatus:          &extractStatus,
		ExtractedAt:            &extractedAt,
		ProcessingPayloadJSON:  stringPtr(string(payloadJSON)),
		ProcessingCompletedAt:  timePtr(&completedAt),
	}); err != nil {
		return err
	}
	if s.embeddingSvc != nil && s.embeddingSvc.ShouldTrigger(*fileObj) {
		// 转写已完成即可发送；RAG 索引在后台生成，避免 embedding 故障把录音误标为转写失败。
		s.embeddingSvc.Trigger(*fileObj)
	}
	return nil
}

func (s *Service) openObjectStore(ctx context.Context) (objectstore.Store, error) {
	if s == nil || s.storeProvider == nil {
		return nil, fmt.Errorf("object store provider not configured")
	}
	return s.storeProvider.Open(ctx)
}

func (s *Service) persistAudioProgress(ctx context.Context, fileObj *domainconversation.FileObject, payload audioProcessingPayload, processingStatus string, extractStatus string, startedAt *time.Time, completedAt *time.Time) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	processingReady := false
	errorCode := ""
	errorMessage := ""
	return s.repo.UpdateFileObjectProcessing(ctx, fileObj.UserID, fileObj.FileID, repository.UpdateFileObjectProcessingInput{
		ProcessingStatus:       &processingStatus,
		ProcessingReady:        &processingReady,
		ProcessingErrorCode:    &errorCode,
		ProcessingErrorMessage: &errorMessage,
		ExtractStatus:          &extractStatus,
		ProcessingPayloadJSON:  stringPtr(string(payloadJSON)),
		ProcessingStartedAt:    timePtr(startedAt),
		ProcessingCompletedAt:  timePtr(completedAt),
	})
}

func (s *Service) failAudio(ctx context.Context, fileObj *domainconversation.FileObject, code string, message string) error {
	if err := s.markFileProcessingFailed(ctx, fileObj, code, message); err != nil {
		return err
	}
	return nil
}

func (s *Service) scheduleAudioPoll(ctx context.Context, userID uint, fileID string) {
	go func() {
		timer := time.NewTimer(audioPollDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.enqueueFileProcessing(context.Background(), userID, fileID, 0, ""); err != nil && s.logger != nil {
				s.logger.Warn("enqueue_audio_poll_failed", zap.Uint("user_id", userID), zap.String("file_id", fileID), zap.Error(err))
			}
		}
	}()
}

func (s *Service) recoverAudioProcessing(ctx context.Context) {
	if s == nil || s.repo == nil {
		return
	}
	items, err := s.repo.ListRecoverableAudioFileObjects(ctx, audioRecoveryBatch)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("recover_audio_processing_failed", zap.Error(err))
		}
		return
	}
	for _, item := range items {
		_ = s.enqueueFileProcessing(ctx, item.UserID, item.FileID, 0, "")
	}
}

func parseAudioProcessingPayload(raw string) audioProcessingPayload {
	payload := audioProcessingPayload{Version: 1, Provider: "dashscope", Model: funasr.DefaultModel, Stage: "uploaded"}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &payload)
	}
	return payload
}

func mustMarshalAudioPayload(payload audioProcessingPayload) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func putAudioArtifact(ctx context.Context, store objectstore.Store, path string, data []byte, contentType string) error {
	_, err := store.Put(ctx, path, bytes.NewReader(data), objectstore.PutOptions{SizeBytes: int64(len(data)), ContentType: contentType})
	return err
}

func audioErrorCode(err error) string {
	if errors.Is(err, funasr.ErrNotConfigured) {
		return "transcription_not_configured"
	}
	return "transcription_request_failed"
}

func audioErrorMessage(err error) string {
	if errors.Is(err, funasr.ErrNotConfigured) {
		return "录音转写服务未配置"
	}
	return "录音转写请求失败，请稍后手动重试"
}

func stringPtr(value string) *string { return &value }

func timePtr(value *time.Time) **time.Time { return &value }
