package conversation

import (
	"context"
	"fmt"
	"strings"
	"time"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	domainknowledgebase "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/knowledgebase"
	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/sqlitevec"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/vectorutil"
	"gorm.io/gorm"
)

// 文件向量、BM25 与 SQLite keyword 检索仓储实现。

// fileChunkSearchRow 是原始 SQL 扫描专用的本地类型，携带 gorm column tag 映射相似度列。
type fileChunkSearchRow struct {
	ID         uint      `gorm:"column:id"`
	FileObjID  uint      `gorm:"column:file_obj_id"`
	UserID     uint      `gorm:"column:user_id"`
	ChunkIndex int       `gorm:"column:chunk_index"`
	PageNum    int       `gorm:"column:page_num"`
	CharOffset int       `gorm:"column:char_offset"`
	Content    string    `gorm:"column:content"`
	TokenCount int       `gorm:"column:token_count"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	Similarity float32   `gorm:"column:similarity"`
}

func (r *Repo) searchSQLiteFileChunks(ctx context.Context, userID uint, fileObjIDs []uint, queryEmbedding []float32, embeddingSignature string, topK int) ([]domainconversation.FileChunkSearchResult, error) {
	vector, err := sqlitevec.SerializeFloat32(queryEmbedding)
	if err != nil {
		return nil, err
	}
	uniqueFileObjIDs := make([]uint, 0, len(fileObjIDs))
	seenFileObjIDs := make(map[uint]struct{}, len(fileObjIDs))
	for _, fileObjID := range fileObjIDs {
		if fileObjID == 0 {
			continue
		}
		if _, exists := seenFileObjIDs[fileObjID]; exists {
			continue
		}
		seenFileObjIDs[fileObjID] = struct{}{}
		uniqueFileObjIDs = append(uniqueFileObjIDs, fileObjID)
	}
	if len(uniqueFileObjIDs) == 0 {
		return nil, nil
	}
	// sqlite-vec applies k before the outer JOIN predicates. Resolve the allowed
	// file IDs first so unauthorized nearest neighbours cannot displace valid
	// candidates from the virtual-table result window.
	authorizedFileObjIDs := make([]uint, 0, len(uniqueFileObjIDs))
	if err := r.db.WithContext(ctx).Table("file_chunks").
		Distinct("file_chunks.file_obj_id").
		Where("file_chunks.file_obj_id IN ?", uniqueFileObjIDs).
		Where(`
			file_chunks.user_id = ?
			OR EXISTS (
				SELECT 1
				FROM knowledge_base_files AS kbf
				JOIN knowledge_bases AS kb ON kb.id = kbf.knowledge_base_id
				WHERE kbf.file_object_id = file_chunks.file_obj_id
					AND kb.scope = ?
					AND kb.enabled = ?
			)`, userID, domainknowledgebase.ScopeBuiltin, true).
		Pluck("file_chunks.file_obj_id", &authorizedFileObjIDs).Error; err != nil {
		return nil, translateError(err)
	}
	if len(authorizedFileObjIDs) == 0 {
		return nil, nil
	}
	var rows []fileChunkSearchRow
	query := fmt.Sprintf(`
		SELECT chunks.id, chunks.file_obj_id, chunks.user_id, chunks.chunk_index, chunks.page_num,
		       chunks.char_offset, chunks.content, chunks.token_count, chunks.created_at,
		       (1.0 - vectors.distance) AS similarity
		FROM %s AS vectors
		JOIN "file_chunks" AS chunks
			ON chunks.id = vectors.chunk_id
		WHERE vectors.embedding MATCH ?
			AND vectors.k = ?
			AND vectors.file_obj_id IN ?
			AND vectors.embedding_signature = ?
			AND chunks.embedding_signature = ?
			AND (
				chunks.user_id = ?
				OR EXISTS (
					SELECT 1
					FROM knowledge_base_files AS kbf
					JOIN knowledge_bases AS kb ON kb.id = kbf.knowledge_base_id
					WHERE kbf.file_object_id = chunks.file_obj_id
						AND kb.scope = ?
						AND kb.enabled = ?
				)
			)
		ORDER BY vectors.distance ASC`,
		sqlitevec.FileChunkVectorTable,
	)
	if err := r.db.WithContext(ctx).Raw(
		query,
		vector,
		topK,
		authorizedFileObjIDs,
		embeddingSignature,
		embeddingSignature,
		userID,
		domainknowledgebase.ScopeBuiltin,
		true,
	).Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.FileChunkSearchResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, domainconversation.FileChunkSearchResult{
			FileChunk: domainconversation.FileChunk{
				ID:         row.ID,
				FileObjID:  row.FileObjID,
				UserID:     row.UserID,
				ChunkIndex: row.ChunkIndex,
				PageNum:    row.PageNum,
				CharOffset: row.CharOffset,
				Content:    row.Content,
				TokenCount: row.TokenCount,
				CreatedAt:  row.CreatedAt,
			},
			Similarity: row.Similarity,
		})
	}
	return results, nil
}

// SearchFileChunks 使用向量存储的余弦距离检索最相关的文本分片。
// 返回结果按相似度降序排列，已携带 Similarity 分数以供阈值过滤。
func (r *Repo) SearchFileChunks(ctx context.Context, userID uint, fileObjIDs []uint, queryEmbedding []float32, embeddingSignature string, topK int) ([]domainconversation.FileChunkSearchResult, error) {
	if userID == 0 || len(fileObjIDs) == 0 || len(queryEmbedding) == 0 || strings.TrimSpace(embeddingSignature) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	if r.sqliteDialect() {
		return r.searchSQLiteFileChunks(ctx, userID, fileObjIDs, queryEmbedding, embeddingSignature, topK)
	}
	vec, err := float32SliceToPostgresQueryVector(queryEmbedding)
	if err != nil {
		return nil, err
	}
	candidateLimit := vectorutil.CandidateLimit(topK)
	indexExpression := vectorutil.PostgresIndexExpression("source_chunks.embedding")
	exactExpression := vectorutil.PostgresPaddedExpression("chunks.embedding")
	query := fmt.Sprintf(`
		WITH vector_candidates AS MATERIALIZED (
			SELECT source_chunks.id
			FROM file_chunks AS source_chunks
			WHERE source_chunks.file_obj_id IN ?
				AND source_chunks.embedding_signature = ?
				AND source_chunks.embedding IS NOT NULL
				AND (
					source_chunks.user_id = ?
					OR EXISTS (
						SELECT 1
						FROM knowledge_base_files AS kbf
						JOIN knowledge_bases AS kb ON kb.id = kbf.knowledge_base_id
						WHERE kbf.file_object_id = source_chunks.file_obj_id
							AND kb.scope = ?
							AND kb.enabled = ?
					)
				)
			ORDER BY %s
				<=> subvector(?::vector, 1, %d)::halfvec(%d)
			LIMIT ?
		)
		SELECT chunks.id, chunks.file_obj_id, chunks.user_id, chunks.chunk_index, chunks.page_num,
		       chunks.char_offset, chunks.content, chunks.token_count, chunks.created_at,
		       (1 - (%s <=> ?::vector(%d))) AS similarity
		FROM file_chunks AS chunks
		JOIN vector_candidates AS candidates ON candidates.id = chunks.id
		ORDER BY similarity DESC
		LIMIT ?`,
		indexExpression,
		vectorutil.IndexDimensions,
		vectorutil.IndexDimensions,
		exactExpression,
		vectorutil.MaxDimensions,
	)
	var rows []fileChunkSearchRow
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := vectorutil.ConfigurePostgresCandidateSearch(tx); err != nil {
			return err
		}
		return tx.Raw(
			query,
			fileObjIDs,
			embeddingSignature,
			userID,
			domainknowledgebase.ScopeBuiltin,
			true,
			vec,
			candidateLimit,
			vec,
			topK,
		).Scan(&rows).Error
	}); err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.FileChunkSearchResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, domainconversation.FileChunkSearchResult{
			FileChunk: domainconversation.FileChunk{
				ID:         row.ID,
				FileObjID:  row.FileObjID,
				UserID:     row.UserID,
				ChunkIndex: row.ChunkIndex,
				PageNum:    row.PageNum,
				CharOffset: row.CharOffset,
				Content:    row.Content,
				TokenCount: row.TokenCount,
				CreatedAt:  row.CreatedAt,
			},
			Similarity: row.Similarity,
		})
	}
	return results, nil
}

// BM25SearchFileChunks 使用 PostgreSQL tsvector 全文检索文件分片，中文字符以空格切字作为后备分词策略。
// 返回结果按 ts_rank 降序，Similarity 字段存放归一化后的排名得分（0-1）。
func (r *Repo) BM25SearchFileChunks(ctx context.Context, userID uint, fileObjIDs []uint, query string, topK int) ([]domainconversation.FileChunkSearchResult, error) {
	if userID == 0 || len(fileObjIDs) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	if r.sqliteDialect() {
		return r.keywordSearchFileChunks(ctx, userID, fileObjIDs, query, topK)
	}
	// 中文字符逐字切开，空格分隔后拼成 OR 查询，提高中文召回率
	tsQuery := buildTSQuery(query)
	if tsQuery == "" {
		return nil, nil
	}
	rawQuery := `
		SELECT id, file_obj_id, user_id, chunk_index, page_num, char_offset, content, token_count, created_at,
		       ts_rank(to_tsvector('simple', content), to_tsquery('simple', ?)) AS similarity
		FROM file_chunks
		WHERE file_obj_id IN ?
		  AND (
			  file_chunks.user_id = ?
			  OR EXISTS (
				  SELECT 1
				  FROM knowledge_base_files AS kbf
				  JOIN knowledge_bases AS kb ON kb.id = kbf.knowledge_base_id
				  WHERE kbf.file_object_id = file_chunks.file_obj_id
					AND kb.scope = ?
					AND kb.enabled = ?
			  )
		  )
		  AND to_tsvector('simple', content) @@ to_tsquery('simple', ?)
		ORDER BY similarity DESC
		LIMIT ?`
	var rows []fileChunkSearchRow
	if err := r.db.WithContext(ctx).Raw(
		rawQuery,
		tsQuery,
		fileObjIDs,
		userID,
		domainknowledgebase.ScopeBuiltin,
		true,
		tsQuery,
		topK,
	).Scan(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.FileChunkSearchResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, domainconversation.FileChunkSearchResult{
			FileChunk: domainconversation.FileChunk{
				ID:         row.ID,
				FileObjID:  row.FileObjID,
				UserID:     row.UserID,
				ChunkIndex: row.ChunkIndex,
				PageNum:    row.PageNum,
				CharOffset: row.CharOffset,
				Content:    row.Content,
				TokenCount: row.TokenCount,
				CreatedAt:  row.CreatedAt,
			},
			Similarity: row.Similarity,
		})
	}
	return results, nil
}

func (r *Repo) keywordSearchFileChunks(ctx context.Context, userID uint, fileObjIDs []uint, query string, topK int) ([]domainconversation.FileChunkSearchResult, error) {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		terms = []string{strings.ToLower(strings.TrimSpace(query))}
	}
	dbq := r.db.WithContext(ctx).
		Model(&models.FileChunk{}).
		Where("file_obj_id IN ?", fileObjIDs).
		Where(`
			file_chunks.user_id = ?
			OR EXISTS (
				SELECT 1
				FROM knowledge_base_files AS kbf
				JOIN knowledge_bases AS kb ON kb.id = kbf.knowledge_base_id
				WHERE kbf.file_object_id = file_chunks.file_obj_id
					AND kb.scope = ?
					AND kb.enabled = ?
			)`, userID, domainknowledgebase.ScopeBuiltin, true)
	for _, term := range terms {
		if strings.TrimSpace(term) == "" {
			continue
		}
		dbq = dbq.Where("LOWER(content) LIKE ?", "%"+term+"%")
	}
	rows := make([]models.FileChunk, 0, topK)
	if err := dbq.Order("id ASC").Limit(topK).Find(&rows).Error; err != nil {
		return nil, translateError(err)
	}
	results := make([]domainconversation.FileChunkSearchResult, 0, len(rows))
	for _, row := range rows {
		results = append(results, domainconversation.FileChunkSearchResult{
			FileChunk: domainconversation.FileChunk{
				ID:         row.ID,
				FileObjID:  row.FileObjID,
				UserID:     row.UserID,
				ChunkIndex: row.ChunkIndex,
				PageNum:    row.PageNum,
				CharOffset: row.CharOffset,
				Content:    row.Content,
				TokenCount: row.TokenCount,
				CreatedAt:  row.CreatedAt,
			},
			Similarity: 0.5,
		})
	}
	return results, nil
}

// buildTSQuery 将查询字符串转换为 PostgreSQL tsquery 格式。
// 中文字符逐字展开，ASCII 单词保留，用 | 连接（OR 语义）。
func buildTSQuery(query string) string {
	var tokens []string
	var wordBuf strings.Builder
	for _, r := range strings.TrimSpace(query) {
		if r > 0x2E7F { // CJK 及更宽字符：单字为 token
			if wordBuf.Len() > 0 {
				tokens = append(tokens, wordBuf.String())
				wordBuf.Reset()
			}
			tokens = append(tokens, string(r))
		} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			wordBuf.WriteRune(r)
		} else {
			if wordBuf.Len() > 0 {
				tokens = append(tokens, wordBuf.String())
				wordBuf.Reset()
			}
		}
	}
	if wordBuf.Len() > 0 {
		tokens = append(tokens, wordBuf.String())
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " | ")
}
