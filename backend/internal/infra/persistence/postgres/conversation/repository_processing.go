package conversation

import (
	"context"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
)

func (r *Repo) UpdateFileObjectProcessingState(ctx context.Context, item *domainconversation.FileObjectProcessing) error {
	if item == nil {
		return nil
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("id = ? AND user_id = ?", item.FileObjectID, item.UserID).
		Updates(fileObjectProcessingStateUpdates(item))
	if result.Error != nil {
		return translateError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Repo) GetFileObjectProcessingByObjectID(ctx context.Context, fileObjID uint) (*domainconversation.FileObjectProcessing, error) {
	var item models.FileObject
	if err := r.db.WithContext(ctx).
		Where("id = ?", fileObjID).
		First(&item).Error; err != nil {
		return nil, err
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

func (r *Repo) CompareAndSwapTranscriptRevision(ctx context.Context, userID uint, fileID string, expectedRevision int) (bool, error) {
	if userID == 0 || fileID == "" || expectedRevision < 1 {
		return false, nil
	}
	const processingPayloadColumn = "processing_payload_json"
	whereRevision := "COALESCE((processing_payload_json::jsonb ->> 'transcriptRevision')::int, 1) = ?"
	updatedPayload := gorm.Expr(
		"jsonb_set(COALESCE(NULLIF(processing_payload_json, ''), '{}')::jsonb, '{transcriptRevision}', to_jsonb(?::int), true)::text",
		expectedRevision+1,
	)
	if r.sqliteDialect() {
		whereRevision = "COALESCE(CAST(json_extract(processing_payload_json, '$.transcriptRevision') AS INTEGER), 1) = ?"
		updatedPayload = gorm.Expr(
			"json_set(CASE WHEN NULLIF(processing_payload_json, '') IS NULL THEN '{}' ELSE processing_payload_json END, '$.transcriptRevision', ?)",
			expectedRevision+1,
		)
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ? AND status = ? AND file_category = ?", userID, fileID, "active", "audio").
		Where(whereRevision, expectedRevision).
		Updates(map[string]interface{}{
			processingPayloadColumn: updatedPayload,
			"updated_at":            time.Now(),
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

func fileObjectProcessingUpdates(input repository.UpdateFileObjectProcessingInput) map[string]interface{} {
	updates := make(map[string]interface{})
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

func nullableTimeValue(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return *value
}
