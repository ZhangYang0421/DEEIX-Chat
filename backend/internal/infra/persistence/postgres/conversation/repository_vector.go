package conversation

import (
	"context"
	"fmt"

	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/sqlitevec"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/vectorutil"
)

// 文件与消息向量存储能力及向量序列化公共实现。

// float32SliceToPostgresVector 按模型原始维度序列化 PostgreSQL 向量。
func float32SliceToPostgresVector(v []float32) (string, error) {
	return vectorutil.PostgresLiteral(v)
}

// float32SliceToPostgresQueryVector 将查询向量补齐到统一比较维度。
func float32SliceToPostgresQueryVector(v []float32) (string, error) {
	return vectorutil.PostgresPaddedLiteral(v)
}

func (r *Repo) VectorStoreAvailable(ctx context.Context) (bool, error) {
	if r.sqliteDialect() {
		return sqlitevec.Available(ctx, r.db)
	}
	expectedType := "vector"
	type availabilityCheck struct {
		query string
		args  []any
	}
	columnQuery := `SELECT EXISTS (
			SELECT 1 FROM pg_attribute AS attribute
			JOIN pg_class AS relation ON relation.oid = attribute.attrelid
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = current_schema()
				AND relation.relname = ?
				AND attribute.attname = 'embedding'
				AND attribute.attnum > 0
				AND NOT attribute.attisdropped
				AND format_type(attribute.atttypid, attribute.atttypmod) = ?
		)`
	indexQuery := `SELECT EXISTS (
		SELECT 1
		FROM pg_index AS index_status
		JOIN pg_class AS index_relation ON index_relation.oid = index_status.indexrelid
		JOIN pg_namespace AS namespace ON namespace.oid = index_relation.relnamespace
		WHERE namespace.nspname = current_schema()
			AND index_relation.relname = ?
			AND index_status.indisvalid
			AND lower(pg_get_indexdef(index_status.indexrelid)) LIKE '% using hnsw %'
			AND lower(pg_get_indexdef(index_status.indexrelid)) LIKE ?
			AND lower(pg_get_indexdef(index_status.indexrelid)) LIKE '%vector_dims(%'
			AND lower(pg_get_indexdef(index_status.indexrelid)) LIKE '%halfvec_cosine_ops%'
	)`
	indexPattern := fmt.Sprintf("%%::halfvec(%d)%%", vectorutil.IndexDimensions)
	checks := []availabilityCheck{
		{query: `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`},
		{query: columnQuery, args: []any{"file_chunks", expectedType}},
		{query: columnQuery, args: []any{"chat_message_chunks", expectedType}},
		{query: columnQuery, args: []any{"user_memories", expectedType}},
		{query: indexQuery, args: []any{"idx_file_chunks_embedding", indexPattern}},
		{query: indexQuery, args: []any{"idx_chat_message_chunks_embedding", indexPattern}},
		{query: indexQuery, args: []any{"idx_user_memories_embedding", indexPattern}},
	}
	for _, check := range checks {
		available := false
		if err := r.db.WithContext(ctx).Raw(check.query, check.args...).Scan(&available).Error; err != nil {
			return false, translateError(err)
		}
		if !available {
			return false, nil
		}
	}
	return true, nil
}
