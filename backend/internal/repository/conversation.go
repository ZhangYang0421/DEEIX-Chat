package repository

import (
	"context"

	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
)

// ConversationRepository 定义 conversation 编排层真正调用的聚合仓储能力。
// 上传、压缩、embedding、处理流水线与 RAG 各自持有独立仓储接口，由组合根装配；
// 这里只嵌入 conversation 自身用到的能力，其中 UploadRepository 同时服务于构造函数内部装配的上传服务。
type ConversationRepository interface {
	ConversationMetadataRepository
	ConversationForkRepository
	MessageRepository
	MessageFeedbackRepository
	ConversationTraceRepository
	ContextArtifactRepository
	MessageEmbeddingRepository
	ModerationFileRepository
	FileBatchRepository
	UploadRepository
	FileEmbeddingArtifactsRepository
	ConversationSettingsRepository
	// 文件处理状态只在读取附件状态、复制分享文件和保存音频转写修订时用到这些能力。
	GetFileObjectProcessingByObjectID(ctx context.Context, fileObjID uint) (*domainconversation.FileObjectProcessing, error)
	CompareAndSwapTranscriptRevision(ctx context.Context, userID uint, fileID string, expectedRevision int) (bool, error)
	CloneFileObjectProcessingState(ctx context.Context, sourceFileObjID uint, targetFileObjID uint, userID uint) error
}
