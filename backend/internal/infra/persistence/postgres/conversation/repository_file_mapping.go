package conversation

import (
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
)

// 文件对象、分片、配额与处理状态的领域/持久化映射 helper。

func toFileObjectDomain(item models.FileObject) domainconversation.FileObject {
	return domainconversation.FileObject{
		ID:                     item.ID,
		FileID:                 item.FileID,
		UserID:                 item.UserID,
		Purpose:                item.Purpose,
		FileName:               item.FileName,
		MimeType:               item.MimeType,
		DetectedMIME:           item.DetectedMIME,
		FileCategory:           item.FileCategory,
		SizeBytes:              item.SizeBytes,
		SHA256:                 item.SHA256,
		StoragePath:            item.StoragePath,
		Status:                 item.Status,
		LastAccessedAt:         item.LastAccessedAt,
		ExpiresAt:              item.ExpiresAt,
		ProcessingStatus:       item.ProcessingStatus,
		ProcessingReady:        item.ProcessingReady,
		ProcessingErrorCode:    item.ProcessingErrorCode,
		ProcessingErrorMessage: item.ProcessingErrorMessage,
		ExtractStatus:          item.ExtractStatus,
		ExtractEngine:          item.ExtractEngine,
		ExtractStoragePath:     item.ExtractStoragePath,
		ExtractChars:           item.ExtractChars,
		ExtractPages:           item.ExtractPages,
		PreviewText:            item.PreviewText,
		OCRUsed:                item.OCRUsed,
		RAGReady:               item.RAGReady,
		RAGReason:              item.RAGReason,
		EmbedStatus:            item.EmbedStatus,
		EmbedSignature:         item.EmbedSignature,
		EmbedError:             item.EmbedError,
		PageCount:              item.PageCount,
		ChunkCount:             item.ChunkCount,
		ExtractorVersion:       item.ExtractorVersion,
		ExtractedAt:            item.ExtractedAt,
		ProcessingPayloadJSON:  item.ProcessingPayloadJSON,
		ProcessingStartedAt:    item.ProcessingStartedAt,
		ProcessingCompletedAt:  item.ProcessingCompletedAt,
		RagOptOut:              item.RagOptOut,
		CreatedAt:              item.CreatedAt,
		UpdatedAt:              item.UpdatedAt,
	}
}

func toFileObjectDomains(items []models.FileObject) []domainconversation.FileObject {
	results := make([]domainconversation.FileObject, 0, len(items))
	for _, item := range items {
		results = append(results, toFileObjectDomain(item))
	}
	return results
}

func toFileObjectModel(item *domainconversation.FileObject) models.FileObject {
	if item == nil {
		return models.FileObject{}
	}
	return models.FileObject{
		BaseModel:              models.BaseModel{ID: item.ID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt},
		FileID:                 item.FileID,
		UserID:                 item.UserID,
		Purpose:                item.Purpose,
		FileName:               item.FileName,
		MimeType:               item.MimeType,
		DetectedMIME:           item.DetectedMIME,
		FileCategory:           item.FileCategory,
		SizeBytes:              item.SizeBytes,
		SHA256:                 item.SHA256,
		StoragePath:            item.StoragePath,
		Status:                 item.Status,
		LastAccessedAt:         item.LastAccessedAt,
		ExpiresAt:              item.ExpiresAt,
		ProcessingStatus:       item.ProcessingStatus,
		ProcessingReady:        item.ProcessingReady,
		ProcessingErrorCode:    item.ProcessingErrorCode,
		ProcessingErrorMessage: item.ProcessingErrorMessage,
		ExtractStatus:          item.ExtractStatus,
		ExtractEngine:          item.ExtractEngine,
		ExtractStoragePath:     item.ExtractStoragePath,
		ExtractChars:           item.ExtractChars,
		ExtractPages:           item.ExtractPages,
		PreviewText:            item.PreviewText,
		OCRUsed:                item.OCRUsed,
		RAGReady:               item.RAGReady,
		RAGReason:              item.RAGReason,
		EmbedStatus:            item.EmbedStatus,
		EmbedSignature:         item.EmbedSignature,
		EmbedError:             item.EmbedError,
		PageCount:              item.PageCount,
		ChunkCount:             item.ChunkCount,
		ExtractorVersion:       item.ExtractorVersion,
		ExtractedAt:            item.ExtractedAt,
		ProcessingPayloadJSON:  item.ProcessingPayloadJSON,
		ProcessingStartedAt:    item.ProcessingStartedAt,
		ProcessingCompletedAt:  item.ProcessingCompletedAt,
		RagOptOut:              item.RagOptOut,
	}
}

func toStorageQuotaDomain(item models.UserStorageQuota) domainconversation.StorageQuota {
	return domainconversation.StorageQuota{
		ID:            item.ID,
		UserID:        item.UserID,
		QuotaBytes:    item.QuotaBytes,
		UsedBytes:     item.UsedBytes,
		ReservedBytes: item.ReservedBytes,
		CreatedAt:     item.CreatedAt,
		UpdatedAt:     item.UpdatedAt,
	}
}

func toFileChunkModel(item *domainconversation.FileChunk) models.FileChunk {
	if item == nil {
		return models.FileChunk{}
	}
	return models.FileChunk{
		FileObjID:          item.FileObjID,
		UserID:             item.UserID,
		ChunkIndex:         item.ChunkIndex,
		PageNum:            item.PageNum,
		CharOffset:         item.CharOffset,
		Content:            item.Content,
		TokenCount:         item.TokenCount,
		EmbeddingSignature: item.EmbeddingSignature,
		CreatedAt:          item.CreatedAt,
	}
}

func toFileObjectProcessingStateDomain(item models.FileObject) domainconversation.FileObjectProcessing {
	return domainconversation.FileObjectProcessing{
		ID:                 item.ID,
		FileObjectID:       item.ID,
		UserID:             item.UserID,
		DetectedMIME:       item.DetectedMIME,
		FileCategory:       item.FileCategory,
		ProcessingStatus:   item.ProcessingStatus,
		ExtractStatus:      item.ExtractStatus,
		ExtractEngine:      item.ExtractEngine,
		ExtractStoragePath: item.ExtractStoragePath,
		ExtractChars:       item.ExtractChars,
		ExtractPages:       item.ExtractPages,
		PreviewText:        item.PreviewText,
		OCRUsed:            item.OCRUsed,
		RAGReady:           item.RAGReady,
		RAGReason:          item.RAGReason,
		ErrorCode:          item.ProcessingErrorCode,
		ErrorMessage:       item.ProcessingErrorMessage,
		ExtractorVersion:   item.ExtractorVersion,
		PayloadJSON:        item.ProcessingPayloadJSON,
		StartedAt:          item.ProcessingStartedAt,
		CompletedAt:        item.ProcessingCompletedAt,
		CreatedAt:          item.CreatedAt,
		UpdatedAt:          item.UpdatedAt,
	}
}

func fileObjectProcessingStateUpdates(item *domainconversation.FileObjectProcessing) map[string]interface{} {
	if item == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"detected_mime":            item.DetectedMIME,
		"file_category":            item.FileCategory,
		"processing_status":        item.ProcessingStatus,
		"extract_status":           item.ExtractStatus,
		"extract_engine":           item.ExtractEngine,
		"extract_storage_path":     item.ExtractStoragePath,
		"extract_chars":            item.ExtractChars,
		"extract_pages":            item.ExtractPages,
		"preview_text":             item.PreviewText,
		"ocr_used":                 item.OCRUsed,
		"rag_ready":                item.RAGReady,
		"rag_reason":               item.RAGReason,
		"processing_error_code":    item.ErrorCode,
		"processing_error_message": item.ErrorMessage,
		"extractor_version":        item.ExtractorVersion,
		"processing_payload_json":  item.PayloadJSON,
		"processing_started_at":    item.StartedAt,
		"processing_completed_at":  item.CompletedAt,
		"updated_at":               time.Now(),
	}
}
