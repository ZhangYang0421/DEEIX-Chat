package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/sqlitevec"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 文件 embedding 生命周期、分片工件及重建状态仓储实现。

// ClaimFileEmbedding 原子领取指定向量空间的文件任务。
// 同一签名已经处于 processing/ready 时不会重复领取；切换向量空间后允许新任务接管。
func (r *Repo) ClaimFileEmbedding(ctx context.Context, userID uint, fileID string, embeddingSignature string) (bool, error) {
	fileID = strings.TrimSpace(fileID)
	embeddingSignature = strings.TrimSpace(embeddingSignature)
	if fileID == "" || embeddingSignature == "" {
		return false, repository.ErrInvalidInput
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ? AND status = ?", userID, fileID, "active").
		Where("NOT (embed_signature = ? AND embed_status IN ?)", embeddingSignature, []string{"processing", "ready"}).
		Updates(map[string]interface{}{
			"embed_status":    "processing",
			"embed_signature": embeddingSignature,
			"embed_error":     "",
		})
	return result.RowsAffected > 0, translateError(result.Error)
}

// UpdateFileObjectEmbedStatus 仅更新仍属于指定向量空间任务的文件状态。
// 同时同步 RAG 可用状态，确保 Embedding 就绪后才允许该文件参与语义检索。
func (r *Repo) UpdateFileObjectEmbedStatus(ctx context.Context, userID uint, fileID string, embeddingSignature string, status string, embedErr string) (bool, error) {
	ragReady, ragReason := embeddingRAGState(status)
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND file_id = ? AND status = ? AND embed_signature = ?", userID, fileID, "active", strings.TrimSpace(embeddingSignature)).
		Updates(map[string]interface{}{
			"embed_status": status,
			"embed_error":  embedErr,
			"rag_ready":    ragReady,
			"rag_reason":   ragReason,
		})
	return result.RowsAffected > 0, translateError(result.Error)
}

func embeddingRAGState(status string) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready":
		return true, "ready"
	case "processing":
		return false, "embedding_processing"
	case "failed":
		return false, "embedding_failed"
	case "stale":
		return false, "embedding_stale"
	default:
		return false, "embedding_pending"
	}
}

// UpdateFileObjectChunkCount 在 embedding 完成后更新分片数量。
func (r *Repo) UpdateFileObjectChunkCount(ctx context.Context, fileObjID uint, embeddingSignature string, chunkCount int) (bool, error) {
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("id = ? AND status = ? AND embed_signature = ?", fileObjID, "active", strings.TrimSpace(embeddingSignature)).
		Update("chunk_count", chunkCount)
	return result.RowsAffected > 0, translateError(result.Error)
}

// CloneFileEmbeddingArtifacts 复用已完成 embedding 的文件分片到新的逻辑别名文件。
// 若目标环境不支持 embedding 列复制，调用方应回退到重新异步 embedding。
func (r *Repo) CloneFileEmbeddingArtifacts(ctx context.Context, source *domainconversation.FileObject, target *domainconversation.FileObject) error {
	if source == nil || target == nil {
		return nil
	}
	sourceEntity := toFileObjectModel(source)
	targetEntity := toFileObjectModel(target)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.FileObject{}).
			Where("id = ?", targetEntity.ID).
			Updates(map[string]interface{}{
				"embed_status":    "ready",
				"embed_signature": sourceEntity.EmbedSignature,
				"embed_error":     "",
				"rag_ready":       true,
				"rag_reason":      "ready",
				"page_count":      sourceEntity.PageCount,
				"chunk_count":     sourceEntity.ChunkCount,
				"extracted_at":    sourceEntity.ExtractedAt,
			}).Error; err != nil {
			return translateError(err)
		}
		if r.sqliteDialect() {
			if err := deleteSQLiteFileChunkVectorsByFile(tx, targetEntity.ID); err != nil {
				return err
			}
		}
		if err := tx.Where("file_obj_id = ?", targetEntity.ID).Delete(&models.FileChunk{}).Error; err != nil {
			return translateError(err)
		}
		if r.sqliteDialect() {
			if err := tx.Exec(
				`INSERT INTO "file_chunks" ("file_obj_id", "user_id", "chunk_index", "page_num", "char_offset", "content", "token_count", "embedding_signature", "created_at")
				 SELECT ?, ?, "chunk_index", "page_num", "char_offset", "content", "token_count", "embedding_signature", CURRENT_TIMESTAMP
				 FROM "file_chunks"
				 WHERE "file_obj_id" = ?`,
				targetEntity.ID,
				targetEntity.UserID,
				sourceEntity.ID,
			).Error; err != nil {
				return translateError(err)
			}
			result := tx.Exec(
				fmt.Sprintf(`INSERT INTO %s (chunk_id, user_id, file_obj_id, embedding_signature, embedding)
					SELECT target_chunks.id, ?, ?, target_chunks.embedding_signature, source_vectors.embedding
					FROM "file_chunks" AS source_chunks
					JOIN "file_chunks" AS target_chunks
						ON target_chunks.file_obj_id = ?
						AND target_chunks.chunk_index = source_chunks.chunk_index
					JOIN %s AS source_vectors
						ON source_vectors.chunk_id = source_chunks.id
					WHERE source_chunks.file_obj_id = ?`,
					sqlitevec.FileChunkVectorTable,
					sqlitevec.FileChunkVectorTable,
				),
				targetEntity.UserID,
				targetEntity.ID,
				targetEntity.ID,
				sourceEntity.ID,
			)
			if err := result.Error; err != nil {
				return translateError(err)
			}
			if sourceEntity.ChunkCount > 0 && result.RowsAffected != int64(sourceEntity.ChunkCount) {
				return fmt.Errorf("sqlite file vector copy mismatch: source_chunks=%d copied_vectors=%d", sourceEntity.ChunkCount, result.RowsAffected)
			}
			return nil
		}
		return tx.Exec(
			`INSERT INTO "file_chunks" ("file_obj_id", "user_id", "chunk_index", "page_num", "char_offset", "content", "token_count", "embedding_signature", "embedding", "created_at")
			 SELECT ?, ?, "chunk_index", "page_num", "char_offset", "content", "token_count", "embedding_signature", "embedding", NOW()
			 FROM "file_chunks"
			 WHERE "file_obj_id" = ?`,
			targetEntity.ID,
			targetEntity.UserID,
			sourceEntity.ID,
		).Error
	})
}

// ReplaceFileChunks 仅在文件任务仍属于指定向量空间时替换全部分片。
// 文件行锁使配置切换后的新任务领取与旧任务发布按顺序完成，避免旧向量覆盖新向量。
func (r *Repo) ReplaceFileChunks(ctx context.Context, fileObjID uint, embeddingSignature string, chunks []domainconversation.FileChunk, embeddings [][]float32) (bool, error) {
	if len(chunks) != len(embeddings) {
		return false, fmt.Errorf("embedding count mismatch: chunks=%d embeddings=%d", len(chunks), len(embeddings))
	}
	embeddingSignature = strings.TrimSpace(embeddingSignature)
	if fileObjID == 0 || embeddingSignature == "" {
		return false, repository.ErrInvalidInput
	}
	published := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var file models.FileObject
		claim := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("id = ? AND status = ? AND embed_status = ? AND embed_signature = ?", fileObjID, "active", "processing", embeddingSignature).
			Take(&file)
		if errors.Is(claim.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if claim.Error != nil {
			return translateError(claim.Error)
		}
		entities := make([]models.FileChunk, 0, len(chunks))
		for i := range chunks {
			if strings.TrimSpace(chunks[i].EmbeddingSignature) != embeddingSignature {
				return fmt.Errorf("chunk embedding signature mismatch")
			}
			entities = append(entities, toFileChunkModel(&chunks[i]))
		}
		if r.sqliteDialect() {
			if err := deleteSQLiteFileChunkVectorsByFile(tx, fileObjID); err != nil {
				return err
			}
		}
		// 删除旧分片
		if err := tx.Where("file_obj_id = ?", fileObjID).Delete(&models.FileChunk{}).Error; err != nil {
			return translateError(err)
		}
		if len(entities) == 0 {
			published = true
			return nil
		}
		// 插入新分片
		if err := tx.Create(&entities).Error; err != nil {
			return translateError(err)
		}
		if r.sqliteDialect() {
			if err := insertSQLiteFileChunkVectors(tx, entities, embeddings); err != nil {
				return err
			}
			published = true
			return nil
		}
		// 更新 embedding（通过 raw SQL 写入 vector 值）
		for i, chunk := range entities {
			if len(embeddings[i]) == 0 {
				return fmt.Errorf("empty embedding vector at chunk %d", i)
			}
			vec, err := float32SliceToPostgresVector(embeddings[i])
			if err != nil {
				return err
			}
			if err := tx.Exec(
				`UPDATE "file_chunks" SET embedding = ? WHERE id = ?`,
				vec, chunk.ID,
			).Error; err != nil {
				return translateError(err)
			}
		}
		published = true
		return nil
	})
	return published, err
}

func deleteSQLiteFileChunkVectorsByFile(tx *gorm.DB, fileObjID uint) error {
	return translateError(tx.Exec(
		fmt.Sprintf(`DELETE FROM %s WHERE chunk_id IN (
			SELECT id FROM "file_chunks" WHERE file_obj_id = ?
		)`, sqlitevec.FileChunkVectorTable),
		fileObjID,
	).Error)
}

func insertSQLiteFileChunkVectors(tx *gorm.DB, entities []models.FileChunk, embeddings [][]float32) error {
	if len(entities) != len(embeddings) {
		return fmt.Errorf("embedding count mismatch: chunks=%d embeddings=%d", len(entities), len(embeddings))
	}
	for i, chunk := range entities {
		if len(embeddings[i]) == 0 {
			return fmt.Errorf("empty embedding vector at chunk %d", i)
		}
		vector, err := sqlitevec.SerializeFloat32(embeddings[i])
		if err != nil {
			return err
		}
		if err = tx.Exec(
			fmt.Sprintf(`INSERT INTO %s (chunk_id, user_id, file_obj_id, embedding_signature, embedding) VALUES (?, ?, ?, ?, ?)`, sqlitevec.FileChunkVectorTable),
			chunk.ID,
			chunk.UserID,
			chunk.FileObjID,
			chunk.EmbeddingSignature,
			vector,
		).Error; err != nil {
			return translateError(err)
		}
	}
	return nil
}

// MarkEmbeddedFilesStale 将缺少当前向量空间签名分片的 ready/processing 文件标记为 stale。
func (r *Repo) MarkEmbeddedFilesStale(ctx context.Context, activeSignature string) (int64, error) {
	activeSignature = strings.TrimSpace(activeSignature)
	if activeSignature == "" {
		return 0, repository.ErrInvalidInput
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("embed_status IN ? AND status = ?", []string{"ready", "processing"}, "active").
		Where(`NOT EXISTS (
			SELECT 1
			FROM file_chunks
			WHERE file_chunks.file_obj_id = file_objects.id
				AND file_chunks.embedding_signature = ?
		)`, activeSignature).
		Updates(map[string]interface{}{
			"embed_status": "stale",
			"embed_error":  "embedding configuration changed, reindex required",
			"rag_ready":    false,
			"rag_reason":   "embedding_stale",
		})
	return result.RowsAffected, translateError(result.Error)
}

// CountFilesByEmbedStatus 统计指定 embed_status 的文件数量。
func (r *Repo) CountFilesByEmbedStatus(ctx context.Context, status string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("embed_status = ? AND status = ?", status, "active").
		Count(&count).Error
	return count, translateError(err)
}

// MarkTimedOutFileEmbeddingsFailed 将长时间停留在向量化中的文件标记为失败。
func (r *Repo) MarkTimedOutFileEmbeddingsFailed(ctx context.Context, userID uint, cutoff time.Time, message string) (int64, error) {
	if message == "" {
		message = "向量化超时"
	}
	result := r.db.WithContext(ctx).
		Model(&models.FileObject{}).
		Where("user_id = ? AND status = ? AND embed_status = ? AND updated_at < ?", userID, "active", "processing", cutoff).
		Updates(map[string]interface{}{
			"embed_status":             "failed",
			"embed_error":              truncateText(message, 255),
			"rag_ready":                false,
			"rag_reason":               "embedding_failed",
			"processing_status":        gorm.Expr("CASE WHEN processing_status = ? THEN ? ELSE processing_status END", "embedding", "ready"),
			"processing_ready":         gorm.Expr("CASE WHEN processing_status = ? THEN ? ELSE processing_ready END", "embedding", true),
			"processing_error_code":    gorm.Expr("CASE WHEN processing_status = ? THEN ? ELSE processing_error_code END", "embedding", "embed_failed"),
			"processing_error_message": gorm.Expr("CASE WHEN processing_status = ? THEN ? ELSE processing_error_message END", "embedding", truncateText(message, 255)),
		})
	return result.RowsAffected, translateError(result.Error)
}

// ListFilesForReindex 分页返回需要重建向量的文件（embed_status 为 none、stale 或 failed）。
func (r *Repo) ListFilesForReindex(ctx context.Context, limit int, afterID uint) ([]domainconversation.FileObject, error) {
	if limit <= 0 {
		limit = 50
	}
	var entities []models.FileObject
	err := r.db.WithContext(ctx).
		Where("id > ? AND embed_status IN ? AND status = ?", afterID, []string{"none", "stale", "failed"}, "active").
		Order("id ASC").
		Limit(limit).
		Find(&entities).Error
	if err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.FileObject, 0, len(entities))
	for i := range entities {
		results = append(results, toFileObjectDomain(entities[i]))
	}
	return results, nil
}
