package conversation

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/dberror"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
)

func translateError(err error) error {
	return dberror.Translate(err)
}

func (r *Repo) UpdateFileObjectProcessingState(ctx context.Context, item *domainconversation.FileObjectProcessing) error {
	if item == nil {
		return nil
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("id = ? AND user_id = ?", item.FileObjectID, item.UserID).
		Updates(fileObjectProcessingStateUpdates(item))
	if result.Error != nil {
		return dberror.Translate(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Repo) UpdateClaimedFileObjectProcessingState(
	ctx context.Context,
	item *domainconversation.FileObjectProcessing,
	attemptID string,
) (bool, error) {
	if item == nil || attemptID == "" {
		return false, nil
	}
	updates := fileObjectProcessingStateUpdates(item)
	if item.ProcessingStatus == "ready" || item.ProcessingStatus == "failed" {
		updates["processing_attempt_id"] = ""
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("id = ? AND user_id = ? AND processing_attempt_id = ?", item.FileObjectID, item.UserID, attemptID).
		Updates(updates)
	if result.Error != nil {
		return false, dberror.Translate(result.Error)
	}
	return result.RowsAffected > 0, nil
}

func (r *Repo) GetFileObjectProcessingByObjectID(ctx context.Context, fileObjID uint) (*domainconversation.FileObjectProcessing, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("id = ?", fileObjID).
		First(&item).Error; err != nil {
		return nil, dberror.Translate(err)
	}
	result := toFileObjectProcessingStateDomain(item)
	return &result, nil
}

func (r *Repo) ListRecoverableAudioFileObjects(ctx context.Context, limit int) ([]domainconversation.FileObject, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	items := make([]models.FileObject, 0)
	if err := r.db.WithContext(ctx).
		Where("status = ? AND file_category = ? AND processing_status IN ?", "active", "audio", []string{"uploaded", "queued", "transcribing"}).
		Order("id ASC").
		Limit(limit).
		Find(&items).Error; err != nil {
		return nil, translateError(err)
	}
	result := make([]domainconversation.FileObject, 0, len(items))
	for _, item := range items {
		result = append(result, toFileObjectDomain(item))
	}
	return result, nil
}

// PublishTranscriptRevision atomically publishes a newly written immutable
// transcript revision and invalidates any embedding derived from the old one.
func (r *Repo) PublishTranscriptRevision(
	ctx context.Context,
	userID uint,
	fileID string,
	input repository.PublishTranscriptRevisionInput,
) (bool, error) {
	fileID = strings.TrimSpace(fileID)
	if userID == 0 || fileID == "" || input.ExpectedRevision < 1 {
		return false, repository.ErrInvalidInput
	}
	if input.Revision <= 0 {
		input.Revision = input.ExpectedRevision + 1
	}
	if input.Revision != input.ExpectedRevision+1 ||
		strings.TrimSpace(input.TranscriptJSONPath) == "" ||
		strings.TrimSpace(input.TranscriptMDPath) == "" {
		return false, repository.ErrInvalidInput
	}
	input.TranscriptJSONPath = filepath.ToSlash(strings.TrimSpace(input.TranscriptJSONPath))
	input.TranscriptMDPath = filepath.ToSlash(strings.TrimSpace(input.TranscriptMDPath))
	if input.ExtractChars < 0 {
		input.ExtractChars = 0
	}

	whereRevision := "COALESCE((NULLIF(processing_payload_json, '')::jsonb ->> 'transcriptRevision')::int, 1) = ?"
	updatedPayload := gorm.Expr(
		`jsonb_set(
			jsonb_set(
				jsonb_set(
					COALESCE(NULLIF(processing_payload_json, '')::jsonb, '{}'::jsonb),
					'{transcriptRevision}', to_jsonb(?::int), true),
				'{transcriptJSONPath}', to_jsonb(?::text), true),
			'{transcriptMDPath}', to_jsonb(?::text), true)::text`,
		input.Revision,
		input.TranscriptJSONPath,
		input.TranscriptMDPath,
	)
	if r.sqliteDialect() {
		whereRevision = "COALESCE(CAST(json_extract(CASE WHEN NULLIF(processing_payload_json, '') IS NULL THEN '{}' ELSE processing_payload_json END, '$.transcriptRevision') AS INTEGER), 1) = ?"
		updatedPayload = gorm.Expr(
			`json_set(
				CASE WHEN NULLIF(processing_payload_json, '') IS NULL THEN '{}' ELSE processing_payload_json END,
				'$.transcriptRevision', ?,
				'$.transcriptJSONPath', ?,
				'$.transcriptMDPath', ?)`,
			input.Revision,
			input.TranscriptJSONPath,
			input.TranscriptMDPath,
		)
	}
	const (
		activeStatus           = "active"
		audioCategory          = "audio"
		staleStatus            = "stale"
		embeddingStaleReason   = "embedding_stale"
		embeddingPendingReason = "embedding_pending"
		noneStatus             = "none"
	)
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ? AND status = ? AND file_category = ?", userID, fileID, activeStatus, audioCategory).
		Where(whereRevision, input.ExpectedRevision).
		Updates(map[string]any{
			"processing_payload_json": updatedPayload,
			"extract_storage_path":    input.TranscriptMDPath,
			"extract_chars":           input.ExtractChars,
			"preview_text":            input.PreviewText,
			"rag_ready":               false,
			"embed_status": gorm.Expr(
				"CASE WHEN embed_status IN (?, ?, ?, ?) THEN ? ELSE embed_status END",
				"queued", "processing", "ready", "failed", staleStatus,
			),
			"rag_reason": gorm.Expr(
				"CASE WHEN embed_status = ? THEN ? ELSE ? END",
				noneStatus, embeddingPendingReason, embeddingStaleReason,
			),
			"embed_error": "",
			"updated_at":  time.Now(),
		})
	if result.Error != nil {
		return false, translateError(result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (r *Repo) CloneFileObjectProcessingState(ctx context.Context, sourceFileObjID uint, targetFileObjID uint, userID uint) error {
	if sourceFileObjID == 0 || targetFileObjID == 0 {
		return nil
	}
	source, err := r.GetFileObjectProcessingByObjectID(ctx, sourceFileObjID)
	if err != nil {
		return nil
	}
	now := time.Now()
	copyItem := *source
	copyItem.ID = 0
	copyItem.FileObjectID = targetFileObjID
	copyItem.UserID = userID
	copyItem.CreatedAt = now
	copyItem.UpdatedAt = now
	return r.UpdateFileObjectProcessingState(ctx, &copyItem)
}

// UpdateFileObjectProcessing updates legacy processing fields used by the
// audio transcription pipeline while the newer claim-based worker is active.
func (r *Repo) UpdateFileObjectProcessing(
	ctx context.Context,
	userID uint,
	fileID string,
	input repository.UpdateFileObjectProcessingInput,
) error {
	updates := fileObjectProcessingUpdates(input)
	if len(updates) == 0 {
		return nil
	}
	updates["updated_at"] = time.Now()
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ?", userID, fileID).
		Updates(updates)
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func fileObjectProcessingUpdates(input repository.UpdateFileObjectProcessingInput) map[string]any {
	updates := make(map[string]any)
	if input.ProcessingStatus != nil {
		updates["processing_status"] = *input.ProcessingStatus
	}
	if input.ProcessingReady != nil {
		updates["processing_ready"] = *input.ProcessingReady
	}
	if input.ProcessingErrorCode != nil {
		updates["processing_error_code"] = *input.ProcessingErrorCode
	}
	if input.ProcessingErrorMessage != nil {
		updates["processing_error_message"] = *input.ProcessingErrorMessage
	}
	if input.ExtractStatus != nil {
		updates["extract_status"] = *input.ExtractStatus
	}
	if input.PageCount != nil {
		updates["page_count"] = *input.PageCount
	}
	if input.ExtractorVersion != nil {
		updates["extractor_version"] = *input.ExtractorVersion
	}
	if input.ExtractedAt != nil {
		updates["extracted_at"] = nullableTimeValue(*input.ExtractedAt)
	}
	if input.ProcessingPayloadJSON != nil {
		updates["processing_payload_json"] = *input.ProcessingPayloadJSON
	}
	if input.ProcessingStartedAt != nil {
		updates["processing_started_at"] = nullableTimeValue(*input.ProcessingStartedAt)
	}
	if input.ProcessingCompletedAt != nil {
		updates["processing_completed_at"] = nullableTimeValue(*input.ProcessingCompletedAt)
	}
	return updates
}

func nullableTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func (r *Repo) TryClaimFileObjectProcessing(
	ctx context.Context,
	userID uint,
	fileID string,
	allowRecovery bool,
	extractorVersion string,
	attemptID string,
) (bool, error) {
	if attemptID == "" {
		return false, nil
	}
	claimableStatuses := []string{"queued"}
	if allowRecovery {
		claimableStatuses = append(claimableStatuses, "extracting", "embedding")
	}
	now := time.Now()
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ?", userID, fileID).
		Where("(processing_status IN ? OR (file_category = ? AND processing_status = ?))", claimableStatuses, "audio", "transcribing").
		Updates(map[string]any{
			"processing_status": gorm.Expr(
				"CASE WHEN processing_status = 'transcribing' THEN 'transcribing' ELSE 'extracting' END",
			),
			"processing_ready":         false,
			"processing_error_code":    "",
			"processing_error_message": "",
			"extract_status":           "processing",
			"extractor_version":        extractorVersion,
			"processing_attempt_id":    attemptID,
			"processing_started_at":    gorm.Expr("COALESCE(processing_started_at, ?)", now),
			"processing_completed_at":  nil,
			"updated_at":               now,
		})
	if result.Error != nil {
		return false, dberror.Translate(result.Error)
	}
	return result.RowsAffected > 0, nil
}

func (r *Repo) ResetFileObjectProcessingForRetry(
	ctx context.Context,
	userID uint,
	fileID string,
	attemptID string,
) (bool, error) {
	now := time.Now()
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where(
			"user_id = ? AND file_id = ? AND processing_attempt_id = ? AND processing_status IN ?",
			userID,
			fileID,
			attemptID,
			[]string{"extracting", "embedding"},
		).
		Updates(map[string]any{
			"processing_status":        "queued",
			"processing_ready":         false,
			"processing_error_code":    "",
			"processing_error_message": "",
			"extract_status":           "none",
			"processing_attempt_id":    "",
			"processing_completed_at":  nil,
			"updated_at":               now,
		})
	if result.Error != nil {
		return false, dberror.Translate(result.Error)
	}
	return result.RowsAffected > 0, nil
}
