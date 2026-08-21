package conversation

import (
	"context"
	"fmt"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/sqlitevec"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/vectorutil"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/gorm"
)

// 消息历史 embedding 分片与向量检索仓储实现。

// UpsertMessageChunks 为指定消息写入向量分片（先删旧后插新，再写 embedding）。
func (r *Repo) UpsertMessageChunks(ctx context.Context, chunks []domainconversation.MessageChunk, embeddings [][]float32) error {
	if len(chunks) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 收集需要删除的 messageID 集合（幂等清理）
		seen := make(map[uint]struct{}, len(chunks))
		messageIDs := make([]uint, 0, len(chunks))
		for _, c := range chunks {
			if _, ok := seen[c.MessageID]; !ok {
				messageIDs = append(messageIDs, c.MessageID)
				seen[c.MessageID] = struct{}{}
			}
		}
		if r.sqliteDialect() {
			if err := deleteSQLiteMessageChunkVectorsByMessages(tx, messageIDs); err != nil {
				return err
			}
		}
		if err := tx.Where("message_id IN ?", messageIDs).Delete(&models.MessageChunk{}).Error; err != nil {
			return translateError(err)
		}
		// 插入新分片
		entities := make([]models.MessageChunk, 0, len(chunks))
		for i := range chunks {
			entities = append(entities, models.MessageChunk{
				ConversationID:     chunks[i].ConversationID,
				MessageID:          chunks[i].MessageID,
				UserID:             chunks[i].UserID,
				Role:               chunks[i].Role,
				ChunkIndex:         chunks[i].ChunkIndex,
				Content:            chunks[i].Content,
				TokenCount:         chunks[i].TokenCount,
				EmbeddingSignature: chunks[i].EmbeddingSignature,
			})
		}
		if err := tx.Create(&entities).Error; err != nil {
			return translateError(err)
		}
		if r.sqliteDialect() {
			return insertSQLiteMessageChunkVectors(tx, entities, embeddings)
		}
		// 写入 embedding 向量。
		for i, entity := range entities {
			if i >= len(embeddings) || len(embeddings[i]) == 0 {
				continue
			}
			vec, err := float32SliceToPostgresVector(embeddings[i])
			if err != nil {
				return err
			}
			if err := tx.Exec(`UPDATE "chat_message_chunks" SET embedding = ? WHERE id = ?`, vec, entity.ID).Error; err != nil {
				return translateError(err)
			}
		}
		return nil
	})
}

type messageChunkSearchRow struct {
	ID             uint      `gorm:"column:id"`
	ConversationID uint      `gorm:"column:conversation_id"`
	MessageID      uint      `gorm:"column:message_id"`
	UserID         uint      `gorm:"column:user_id"`
	Role           string    `gorm:"column:role"`
	ChunkIndex     int       `gorm:"column:chunk_index"`
	Content        string    `gorm:"column:content"`
	TokenCount     int       `gorm:"column:token_count"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	Similarity     float64   `gorm:"column:similarity"`
}

func deleteSQLiteMessageChunkVectorsByMessages(tx *gorm.DB, messageIDs []uint) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return translateError(tx.Exec(
		fmt.Sprintf(`DELETE FROM %s WHERE chunk_id IN (
			SELECT id FROM "chat_message_chunks" WHERE message_id IN ?
		)`, sqlitevec.MessageChunkVectorTable),
		messageIDs,
	).Error)
}

func insertSQLiteMessageChunkVectors(tx *gorm.DB, entities []models.MessageChunk, embeddings [][]float32) error {
	for i, chunk := range entities {
		if i >= len(embeddings) || len(embeddings[i]) == 0 {
			continue
		}
		vector, err := sqlitevec.SerializeFloat32(embeddings[i])
		if err != nil {
			return err
		}
		if err = tx.Exec(
			fmt.Sprintf(`INSERT INTO %s (chunk_id, user_id, conversation_id, message_id, embedding_signature, embedding) VALUES (?, ?, ?, ?, ?, ?)`, sqlitevec.MessageChunkVectorTable),
			chunk.ID,
			chunk.UserID,
			chunk.ConversationID,
			chunk.MessageID,
			chunk.EmbeddingSignature,
			vector,
		).Error; err != nil {
			return translateError(err)
		}
	}
	return nil
}

func (r *Repo) searchSQLiteMessageChunks(ctx context.Context, input repository.MessageChunkSearchInput) ([]domainconversation.MessageChunk, error) {
	vector, err := sqlitevec.SerializeFloat32(input.QueryEmbedding)
	if err != nil {
		return nil, err
	}
	query := historicalMessageScopeCTE + fmt.Sprintf(`
		SELECT chunks.id, chunks.conversation_id, chunks.message_id, chunks.user_id, chunks.role,
		       chunks.chunk_index, chunks.content, chunks.token_count, chunks.created_at,
		       (1.0 - vectors.distance) AS similarity
		FROM %s AS vectors
		JOIN "chat_message_chunks" AS chunks
			ON chunks.id = vectors.chunk_id
		WHERE vectors.embedding MATCH ?
			AND vectors.k = ?
			AND vectors.user_id = ?
			AND vectors.conversation_id = ?
			AND vectors.embedding_signature = ?
			AND chunks.embedding_signature = ?
			AND vectors.message_id IN (
				SELECT id
				FROM valid_historical_message_scope
			)
		ORDER BY vectors.distance ASC`,
		sqlitevec.MessageChunkVectorTable,
	)
	args := historicalMessageScopeArgs(input.Scope)
	args = append(args,
		vector,
		input.TopK,
		input.Scope.UserID,
		input.Scope.ConversationID,
		input.EmbeddingSignature,
		input.EmbeddingSignature,
	)
	var rows []messageChunkSearchRow
	if err := r.db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.MessageChunk, 0, len(rows))
	for _, row := range rows {
		if row.Similarity < input.MinSimilarity {
			continue
		}
		results = append(results, domainconversation.MessageChunk{
			ID:             row.ID,
			ConversationID: row.ConversationID,
			MessageID:      row.MessageID,
			UserID:         row.UserID,
			Role:           row.Role,
			ChunkIndex:     row.ChunkIndex,
			Content:        row.Content,
			TokenCount:     row.TokenCount,
			Similarity:     row.Similarity,
			CreatedAt:      row.CreatedAt,
		})
	}
	return results, nil
}

// SearchMessageChunks 在当前活跃分支内按查询向量检索最相关的历史消息分片。
func (r *Repo) SearchMessageChunks(ctx context.Context, input repository.MessageChunkSearchInput) ([]domainconversation.MessageChunk, error) {
	if !input.Scope.Valid() || len(input.QueryEmbedding) == 0 || strings.TrimSpace(input.EmbeddingSignature) == "" || input.TopK <= 0 {
		return nil, nil
	}
	if r.sqliteDialect() {
		return r.searchSQLiteMessageChunks(ctx, input)
	}
	vec, err := float32SliceToPostgresQueryVector(input.QueryEmbedding)
	if err != nil {
		return nil, err
	}
	candidateLimit := vectorutil.CandidateLimit(input.TopK)
	// 候选阶段已经限定当前分支和向量签名，随后再按完整 4096 维向量精确重排。
	indexExpression := vectorutil.PostgresIndexExpression("chunks.embedding")
	exactExpression := vectorutil.PostgresPaddedExpression("chunks.embedding")
	query := historicalMessageScopeCTE + fmt.Sprintf(`,
		vector_candidates AS MATERIALIZED (
			SELECT chunks.id
			FROM chat_message_chunks AS chunks
			WHERE chunks.conversation_id = ?
			  AND chunks.user_id = ?
			  AND chunks.embedding_signature = ?
			  AND chunks.embedding IS NOT NULL
			  AND chunks.message_id IN (SELECT id FROM valid_historical_message_scope)
			ORDER BY %s
				<=> subvector(?::vector, 1, %d)::halfvec(%d)
			LIMIT ?
		)
		SELECT chunks.id, chunks.conversation_id, chunks.message_id, chunks.user_id, chunks.role,
		       chunks.chunk_index, chunks.content, chunks.token_count, chunks.created_at,
		       (1 - (%s <=> ?::vector(%d))) AS similarity
		FROM chat_message_chunks AS chunks
		JOIN vector_candidates AS candidates ON candidates.id = chunks.id
		ORDER BY similarity DESC
		LIMIT ?`,
		indexExpression,
		vectorutil.IndexDimensions,
		vectorutil.IndexDimensions,
		exactExpression,
		vectorutil.MaxDimensions,
	)
	args := historicalMessageScopeArgs(input.Scope)
	args = append(args,
		input.Scope.ConversationID,
		input.Scope.UserID,
		input.EmbeddingSignature,
		vec,
		candidateLimit,
		vec,
		input.TopK,
	)
	var rows []messageChunkSearchRow
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := vectorutil.ConfigurePostgresCandidateSearch(tx); err != nil {
			return err
		}
		return tx.Raw(query, args...).Scan(&rows).Error
	}); err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.MessageChunk, 0, len(rows))
	for _, row := range rows {
		if row.Similarity < input.MinSimilarity {
			continue
		}
		results = append(results, domainconversation.MessageChunk{
			ID:             row.ID,
			ConversationID: row.ConversationID,
			MessageID:      row.MessageID,
			UserID:         row.UserID,
			Role:           row.Role,
			ChunkIndex:     row.ChunkIndex,
			Content:        row.Content,
			TokenCount:     row.TokenCount,
			Similarity:     row.Similarity,
		})
	}
	return results, nil
}
