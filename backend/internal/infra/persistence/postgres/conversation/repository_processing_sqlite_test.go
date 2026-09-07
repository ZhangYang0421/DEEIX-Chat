package conversation

import (
	"context"
	"encoding/json"
	"testing"

	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCompareAndSwapTranscriptRevisionSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:compare_transcript_revision?mode=memory&cache=shared"), &gorm.Config{})
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
		FileID:                "file_transcript_revision",
		UserID:                1,
		FileCategory:          "audio",
		Status:                "active",
		ProcessingPayloadJSON: `{"version":1}`,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create file object: %v", err)
	}

	repo := NewRepo(db)
	locked, err := repo.CompareAndSwapTranscriptRevision(context.Background(), 1, file.FileID, 1)
	if err != nil {
		t.Fatalf("compare-and-swap revision: %v", err)
	}
	if !locked {
		t.Fatal("expected first revision compare-and-swap to succeed")
	}

	var stored models.FileObject
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload file object: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(stored.ProcessingPayloadJSON), &payload); err != nil {
		t.Fatalf("decode processing payload: %v", err)
	}
	if revision, ok := payload["transcriptRevision"].(float64); !ok || revision != 2 {
		t.Fatalf("transcript revision = %#v, want 2", payload["transcriptRevision"])
	}

	locked, err = repo.CompareAndSwapTranscriptRevision(context.Background(), 1, file.FileID, 1)
	if err != nil {
		t.Fatalf("stale compare-and-swap revision: %v", err)
	}
	if locked {
		t.Fatal("expected stale revision compare-and-swap to fail")
	}

	restored, err := repo.SetTranscriptRevisionIfExpected(context.Background(), 1, file.FileID, 2, 1)
	if err != nil {
		t.Fatalf("restore transcript revision: %v", err)
	}
	if !restored {
		t.Fatal("expected revision restore to succeed")
	}

	if restored, err = repo.SetTranscriptRevisionIfExpected(context.Background(), 1, file.FileID, 2, 1); err != nil {
		t.Fatalf("stale revision restore: %v", err)
	} else if restored {
		t.Fatal("expected stale revision restore to fail")
	}
}
