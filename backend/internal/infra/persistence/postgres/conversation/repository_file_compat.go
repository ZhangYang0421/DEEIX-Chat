package conversation

import (
	"strings"

	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/vectorutil"
)

// embeddingRAGState maps embedding lifecycle states to the corresponding RAG
// availability state used by file processing responses.
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

func float32SliceToPostgresVector(v []float32) (string, error) {
	return vectorutil.PostgresLiteral(v)
}

func float32SliceToPostgresQueryVector(v []float32) (string, error) {
	return vectorutil.PostgresPaddedLiteral(v)
}

func buildTSQuery(query string) string {
	var tokens []string
	var wordBuf strings.Builder
	for _, r := range strings.TrimSpace(query) {
		if r > 0x2E7F {
			if wordBuf.Len() > 0 {
				tokens = append(tokens, wordBuf.String())
				wordBuf.Reset()
			}
			tokens = append(tokens, string(r))
		} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			wordBuf.WriteRune(r)
		} else if wordBuf.Len() > 0 {
			tokens = append(tokens, wordBuf.String())
			wordBuf.Reset()
		}
	}
	if wordBuf.Len() > 0 {
		tokens = append(tokens, wordBuf.String())
	}
	return strings.Join(tokens, " | ")
}
