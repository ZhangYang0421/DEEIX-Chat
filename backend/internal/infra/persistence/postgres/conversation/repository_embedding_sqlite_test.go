package conversation

import (
	"context"
	"testing"

	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUpdateFileObjectEmbedStatusSQLiteUpdatesRAGState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:update_embed_status?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("resolve sqlite connection: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := db.AutoMigrate(&models.FileObject{}); err != nil {
		t.Fatalf("migrate file objects: %v", err)
	}
	file := models.FileObject{
		FileID:                "file_embed_status",
		UserID:                1,
		FileCategory:          "audio",
		Status:                "active",
		ProcessingPayloadJSON: `{}`,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create file object: %v", err)
	}

	repo := NewRepo(db)
	if ok, err := repo.UpdateFileObjectEmbedStatus(context.Background(), 1, file.FileID, "", "ready", ""); err != nil {
		t.Fatalf("mark embedding ready: %v", err)
	} else if !ok {
		t.Fatalf("mark embedding ready: no rows affected")
	}

	var stored models.FileObject
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload ready file object: %v", err)
	}
	if !stored.RAGReady || stored.RAGReason != "ready" {
		t.Fatalf("ready state = (%t, %q), want (true, ready)", stored.RAGReady, stored.RAGReason)
	}

	if ok, err := repo.UpdateFileObjectEmbedStatus(context.Background(), 1, file.FileID, "", "failed", "provider error"); err != nil {
		t.Fatalf("mark embedding failed: %v", err)
	} else if !ok {
		t.Fatalf("mark embedding failed: no rows affected")
	}
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload failed file object: %v", err)
	}
	if stored.RAGReady || stored.RAGReason != "embedding_failed" {
		t.Fatalf("failed state = (%t, %q), want (false, embedding_failed)", stored.RAGReady, stored.RAGReason)
	}
}
