package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	models "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/persistence/models"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openProcessingSQLite(t *testing.T, name string) (*gorm.DB, *Repo) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
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
	return db, NewRepo(db)
}

func TestPublishTranscriptRevisionSQLite(t *testing.T) {
	for _, test := range []struct {
		status     string
		wantStatus string
		wantReason string
	}{
		{status: "none", wantStatus: "none", wantReason: "embedding_pending"},
		{status: "queued", wantStatus: "stale", wantReason: "embedding_stale"},
		{status: "processing", wantStatus: "stale", wantReason: "embedding_stale"},
		{status: "ready", wantStatus: "stale", wantReason: "embedding_stale"},
		{status: "failed", wantStatus: "stale", wantReason: "embedding_stale"},
	} {
		t.Run(test.status, func(t *testing.T) {
			db, repo := openProcessingSQLite(t, "publish_transcript_revision_"+test.status)
			payloadJSON := `{"version":1,"rawResultPath":"immutable/result.raw.json","transcriptJSONPath":"old/transcript.json","transcriptMDPath":"old/transcript.md"}`
			file := models.FileObject{
				FileID:                "file_transcript_revision_" + test.status,
				UserID:                1,
				FileCategory:          "audio",
				Status:                "active",
				ProcessingPayloadJSON: payloadJSON,
				ExtractStoragePath:    "old/transcript.md",
				ExtractChars:          3,
				PreviewText:           "old",
				RAGReady:              true,
				RAGReason:             "ready",
				EmbedStatus:           test.status,
				EmbedError:            "old embedding error",
			}
			if err := db.Create(&file).Error; err != nil {
				t.Fatalf("create file object: %v", err)
			}
			input := repository.PublishTranscriptRevisionInput{
				ExpectedRevision:   1,
				Revision:           2,
				TranscriptJSONPath: ".transcripts/uid_1/file/revisions/rev-2-id/transcript.json",
				TranscriptMDPath:   ".transcripts/uid_1/file/revisions/rev-2-id/transcript.md",
				ExtractChars:       42,
				PreviewText:        "new preview",
			}
			published, err := repo.PublishTranscriptRevision(context.Background(), 1, file.FileID, input)
			if err != nil || !published {
				t.Fatalf("publish transcript revision: published=%v err=%v", published, err)
			}
			var stored models.FileObject
			if err := db.First(&stored, file.ID).Error; err != nil {
				t.Fatalf("reload file object: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(stored.ProcessingPayloadJSON), &payload); err != nil {
				t.Fatalf("decode processing payload: %v", err)
			}
			if payload["transcriptRevision"] != float64(2) || payload["transcriptJSONPath"] != input.TranscriptJSONPath || payload["transcriptMDPath"] != input.TranscriptMDPath || payload["rawResultPath"] != "immutable/result.raw.json" {
				t.Fatalf("published processing payload = %#v", payload)
			}
			if stored.ExtractStoragePath != input.TranscriptMDPath || stored.ExtractChars != input.ExtractChars || stored.PreviewText != input.PreviewText {
				t.Fatalf("extract projection = path %q chars %d preview %q", stored.ExtractStoragePath, stored.ExtractChars, stored.PreviewText)
			}
			if stored.RAGReady || stored.RAGReason != test.wantReason || stored.EmbedStatus != test.wantStatus || stored.EmbedError != "" {
				t.Fatalf("RAG/embed state = ready %t reason %q status %q error %q", stored.RAGReady, stored.RAGReason, stored.EmbedStatus, stored.EmbedError)
			}
			staleInput := input
			staleInput.TranscriptJSONPath = "loser/transcript.json"
			staleInput.TranscriptMDPath = "loser/transcript.md"
			if published, err = repo.PublishTranscriptRevision(context.Background(), 1, file.FileID, staleInput); err != nil {
				t.Fatalf("stale transcript publication: %v", err)
			} else if published {
				t.Fatal("expected stale transcript publication to lose CAS")
			}
			if err := db.First(&stored, file.ID).Error; err != nil {
				t.Fatalf("reload after stale transcript publication: %v", err)
			}
			if stored.ExtractStoragePath != input.TranscriptMDPath || strings.Contains(stored.ProcessingPayloadJSON, "loser/transcript") {
				t.Fatalf("stale publication changed published pointers: path=%q payload=%s", stored.ExtractStoragePath, stored.ProcessingPayloadJSON)
			}
		})
	}
}

func TestGetFileObjectProcessingByObjectIDTranslatesNotFound(t *testing.T) {
	_, repo := openProcessingSQLite(t, "processing_not_found")
	if _, err := repo.GetFileObjectProcessingByObjectID(context.Background(), 999); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("GetFileObjectProcessingByObjectID() error = %v, want repository.ErrNotFound", err)
	}
}

func TestTryClaimFileObjectProcessingAllowsAudioTranscribing(t *testing.T) {
	db, repo := openProcessingSQLite(t, "claim_audio_transcribing")
	file := models.FileObject{
		FileID:           "file_audio_transcribing",
		UserID:           1,
		FileCategory:     "audio",
		Status:           "active",
		ProcessingStatus: "transcribing",
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create audio file object: %v", err)
	}

	claimed, err := repo.TryClaimFileObjectProcessing(
		context.Background(), 1, file.FileID, false, "extractor-test", "attempt-audio",
	)
	if err != nil {
		t.Fatalf("claim audio file object: %v", err)
	}
	if !claimed {
		t.Fatal("expected audio transcribing file to be claimable")
	}

	var stored models.FileObject
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload audio file object: %v", err)
	}
	if stored.ProcessingStatus != "transcribing" {
		t.Fatalf("processing status = %q, want transcribing", stored.ProcessingStatus)
	}
	if stored.ProcessingAttemptID != "attempt-audio" {
		t.Fatalf("processing attempt id = %q, want attempt-audio", stored.ProcessingAttemptID)
	}
}

func TestTryClaimFileObjectProcessingRejectsNonAudioTranscribing(t *testing.T) {
	db, repo := openProcessingSQLite(t, "claim_non_audio_transcribing")
	file := models.FileObject{
		FileID:           "file_pdf_transcribing",
		UserID:           1,
		FileCategory:     "pdf",
		Status:           "active",
		ProcessingStatus: "transcribing",
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create non-audio file object: %v", err)
	}

	claimed, err := repo.TryClaimFileObjectProcessing(
		context.Background(), 1, file.FileID, false, "extractor-test", "attempt-pdf",
	)
	if err != nil {
		t.Fatalf("claim non-audio file object: %v", err)
	}
	if claimed {
		t.Fatal("expected non-audio transcribing file not to be claimable")
	}

	var stored models.FileObject
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload non-audio file object: %v", err)
	}
	if stored.ProcessingStatus != "transcribing" {
		t.Fatalf("processing status = %q, want transcribing", stored.ProcessingStatus)
	}
	if stored.ProcessingAttemptID != "" {
		t.Fatalf("processing attempt id = %q, want empty", stored.ProcessingAttemptID)
	}
}

func TestResetFileObjectProcessingForRetryClearsFailureFields(t *testing.T) {
	db, repo := openProcessingSQLite(t, "reset_processing_retry")
	completedAt := time.Now().Add(-time.Minute)
	file := models.FileObject{
		FileID:                 "file_retry",
		UserID:                 1,
		FileCategory:           "pdf",
		Status:                 "active",
		ProcessingStatus:       "extracting",
		ProcessingReady:        true,
		ProcessingErrorCode:    "extract_failed",
		ProcessingErrorMessage: "旧错误信息",
		ExtractStatus:          "failed",
		ProcessingAttemptID:    "attempt-old",
		ProcessingCompletedAt:  &completedAt,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("create retry file object: %v", err)
	}

	reset, err := repo.ResetFileObjectProcessingForRetry(
		context.Background(), 1, file.FileID, "attempt-old",
	)
	if err != nil {
		t.Fatalf("reset file processing: %v", err)
	}
	if !reset {
		t.Fatal("expected file processing reset to succeed")
	}

	var stored models.FileObject
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatalf("reload retry file object: %v", err)
	}
	if stored.ProcessingStatus != "queued" {
		t.Fatalf("processing status = %q, want queued", stored.ProcessingStatus)
	}
	if stored.ProcessingReady {
		t.Fatal("processing ready = true, want false")
	}
	if stored.ProcessingErrorCode != "" {
		t.Fatalf("processing error code = %q, want empty", stored.ProcessingErrorCode)
	}
	if stored.ProcessingErrorMessage != "" {
		t.Fatalf("processing error message = %q, want empty", stored.ProcessingErrorMessage)
	}
	if stored.ExtractStatus != "none" {
		t.Fatalf("extract status = %q, want none", stored.ExtractStatus)
	}
	if stored.ProcessingAttemptID != "" {
		t.Fatalf("processing attempt id = %q, want empty", stored.ProcessingAttemptID)
	}
	if stored.ProcessingCompletedAt != nil {
		t.Fatalf("processing completed at = %v, want nil", stored.ProcessingCompletedAt)
	}
}
func TestTryClaimFileObjectProcessingScopesAudioRecoveryToRequestedFile(t *testing.T) {
	db, repo := openProcessingSQLite(t, "claim_audio_scope")
	target := models.FileObject{
		FileID:           "file_target_queued",
		UserID:           1,
		FileCategory:     "pdf",
		Status:           "active",
		ProcessingStatus: "queued",
	}
	otherUser := models.FileObject{
		FileID:           "file_other_user_transcribing",
		UserID:           2,
		FileCategory:     "audio",
		Status:           "active",
		ProcessingStatus: "transcribing",
	}
	otherFile := models.FileObject{
		FileID:           "file_other_transcribing",
		UserID:           1,
		FileCategory:     "audio",
		Status:           "active",
		ProcessingStatus: "transcribing",
	}
	for _, item := range []*models.FileObject{&target, &otherUser, &otherFile} {
		if err := db.Create(item).Error; err != nil {
			t.Fatalf("create file object %q: %v", item.FileID, err)
		}
	}

	claimed, err := repo.TryClaimFileObjectProcessing(
		context.Background(), 1, target.FileID, false, "extractor-test", "attempt-target",
	)
	if err != nil {
		t.Fatalf("claim target file object: %v", err)
	}
	if !claimed {
		t.Fatal("expected target queued file to be claimable")
	}

	var storedTarget, storedOtherUser, storedOtherFile models.FileObject
	for _, pair := range []struct {
		stored *models.FileObject
		id     uint
	}{
		{stored: &storedTarget, id: target.ID},
		{stored: &storedOtherUser, id: otherUser.ID},
		{stored: &storedOtherFile, id: otherFile.ID},
	} {
		if err := db.First(pair.stored, pair.id).Error; err != nil {
			t.Fatalf("reload file object %d: %v", pair.id, err)
		}
	}
	if storedTarget.ProcessingStatus != "extracting" || storedTarget.ProcessingAttemptID != "attempt-target" {
		t.Fatalf("target processing state = %q/%q, want extracting/attempt-target", storedTarget.ProcessingStatus, storedTarget.ProcessingAttemptID)
	}
	for _, item := range []models.FileObject{storedOtherUser, storedOtherFile} {
		if item.ProcessingStatus != "transcribing" || item.ProcessingAttemptID != "" {
			t.Fatalf("unrelated file %q processing state = %q/%q, want transcribing/empty", item.FileID, item.ProcessingStatus, item.ProcessingAttemptID)
		}
	}
}
