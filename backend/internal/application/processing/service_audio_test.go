package processing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appstorage "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/application/objectstorage"
	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	memorycache "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/cache/memory"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/config"
	portfunasr "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/funasr"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type processingServiceRepositoryCapture struct {
	*processingStateRepositoryStub
	stateUpdates  []*domainconversation.FileObjectProcessing
	legacyUpdates []repository.UpdateFileObjectProcessingInput
}

func (r *processingServiceRepositoryCapture) UpdateFileObjectProcessingState(_ context.Context, state *domainconversation.FileObjectProcessing) error {
	if state != nil {
		copyState := *state
		r.stateUpdates = append(r.stateUpdates, &copyState)
	}
	return nil
}

func (r *processingServiceRepositoryCapture) UpdateFileObjectProcessing(_ context.Context, _ uint, _ string, input repository.UpdateFileObjectProcessingInput) error {
	r.legacyUpdates = append(r.legacyUpdates, input)
	return nil
}

type processingTestStoreProvider struct{}

func (*processingTestStoreProvider) Open(context.Context) (objectstore.Store, error) {
	return nil, nil
}

var _ appstorage.Provider = (*processingTestStoreProvider)(nil)

type processingTestTranscriber struct {
	status *portfunasr.TaskStatus
}

func (*processingTestTranscriber) Submit(context.Context, portfunasr.SubmitInput) (*portfunasr.SubmitResult, error) {
	return &portfunasr.SubmitResult{TaskID: "task-test"}, nil
}

func (t *processingTestTranscriber) GetTask(context.Context, string) (*portfunasr.TaskStatus, error) {
	status := *t.status
	return &status, nil
}

func (*processingTestTranscriber) DownloadJSON(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (*processingTestTranscriber) Configured() bool {
	return true
}

func (*processingTestTranscriber) Model() string {
	return portfunasr.DefaultModel
}

func newProcessingServiceRepositoryCapture(file domainconversation.FileObject) *processingServiceRepositoryCapture {
	return &processingServiceRepositoryCapture{
		processingStateRepositoryStub: &processingStateRepositoryStub{file: file},
	}
}

func TestHandleProcessingMessageSettlesNormalAudioPoll(t *testing.T) {
	workerContext, cancel := context.WithCancel(context.Background())
	defer cancel()

	repo := newProcessingServiceRepositoryCapture(domainconversation.FileObject{
		ID:                    11,
		UserID:                7,
		FileID:                "audio-poll",
		FileName:              "meeting.mp3",
		FileCategory:          "audio",
		ProcessingStatus:      "transcribing",
		ProcessingPayloadJSON: `{"taskID":"task-poll","stage":"polling"}`,
	})
	cache := memorycache.New()
	service := NewServiceWithRuntime(Dependencies{
		Config:     config.NewRuntime(config.Config{}),
		Repository: repo,
		Cache:      cache,
		Logger:     zap.NewNop(),
	})
	service.SetObjectStoreProvider(&processingTestStoreProvider{})
	service.SetAudioTranscriber(&processingTestTranscriber{
		status: &portfunasr.TaskStatus{Status: "RUNNING", Terminal: false},
	})
	service.workerContext = workerContext

	if err := cache.EnqueueFileProcessing(context.Background(), 7, "audio-poll", 0, ""); err != nil {
		t.Fatalf("enqueue audio poll: %v", err)
	}
	messages, err := cache.ReadFileProcessingMessages(context.Background(), "worker")
	if err != nil || len(messages) != 1 {
		t.Fatalf("read audio poll message: messages=%#v err=%v", messages, err)
	}

	service.handleProcessingMessage(workerContext, "worker", messages[0])
	cancel()

	if owned, err := cache.RenewFileProcessingMessageLease(context.Background(), "worker", messages[0]); err != nil || owned {
		t.Fatalf("normal audio poll message was not settled: owned=%v err=%v", owned, err)
	}
	if len(repo.legacyUpdates) == 0 {
		t.Fatal("expected audio polling progress to be persisted")
	}
	last := repo.legacyUpdates[len(repo.legacyUpdates)-1]
	if last.ProcessingStatus == nil || *last.ProcessingStatus != "transcribing" {
		t.Fatalf("audio polling status update = %#v, want transcribing", last.ProcessingStatus)
	}
}

func TestRetryFileProcessingClearsPreviousFailureState(t *testing.T) {
	completedAt := timeNowForProcessingTest()
	repo := newProcessingServiceRepositoryCapture(domainconversation.FileObject{
		ID:                     12,
		UserID:                 7,
		FileID:                 "document-retry",
		FileName:               "document.pdf",
		FileCategory:           "pdf",
		ProcessingStatus:       "failed",
		ProcessingErrorCode:    "extract_failed",
		ProcessingErrorMessage: "previous failure",
		ProcessingCompletedAt:  &completedAt,
	})
	cache := memorycache.New()
	service := NewServiceWithRuntime(Dependencies{
		Config:     config.NewRuntime(config.Config{}),
		Repository: repo,
		Cache:      cache,
	})

	if err := service.RetryFileProcessing(context.Background(), 7, "document-retry"); err != nil {
		t.Fatalf("retry file processing: %v", err)
	}
	if len(repo.stateUpdates) == 0 {
		t.Fatal("expected retry processing state update")
	}
	state := repo.stateUpdates[len(repo.stateUpdates)-1]
	if state.ProcessingStatus != "queued" || state.ProcessingReady {
		t.Fatalf("retry processing state = %#v, want queued and not ready", state)
	}
	if state.ErrorCode != "" || state.ErrorMessage != "" {
		t.Fatalf("retry retained previous failure fields: code=%q message=%q", state.ErrorCode, state.ErrorMessage)
	}
	if state.CompletedAt != nil {
		t.Fatalf("retry completed timestamp = %v, want nil", state.CompletedAt)
	}

	messages, err := cache.ReadFileProcessingMessages(context.Background(), "worker")
	if err != nil || len(messages) != 1 || messages[0].FileID != "document-retry" {
		t.Fatalf("retry queue message = %#v err=%v", messages, err)
	}
}

func TestPollAudioLogsRedactedProviderDetail(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	repo := newProcessingServiceRepositoryCapture(domainconversation.FileObject{
		ID:               13,
		UserID:           7,
		FileID:           "audio-failed",
		FileName:         "meeting.mp3",
		FileCategory:     "audio",
		ProcessingStatus: "transcribing",
	})
	service := NewServiceWithRuntime(Dependencies{
		Config:     config.NewRuntime(config.Config{}),
		Repository: repo,
		Logger:     zap.New(core),
	})
	service.SetAudioTranscriber(&processingTestTranscriber{
		status: &portfunasr.TaskStatus{
			Status:    "FAILED",
			Terminal:  true,
			Succeeded: false,
			Message:   "Audio duration exceeds limit; url=https://provider.example/tasks/1?token=secret; Authorization: Bearer bearer-secret; token=task-secret; path=C:\\internal\\audio.json",
		},
	})

	file := repo.file
	err := service.pollAudio(context.Background(), nil, &file, audioProcessingPayload{TaskID: "task-failed"})
	if err != nil {
		t.Fatalf("poll failed audio: %v", err)
	}

	entries := observed.FilterMessage("audio_transcription_task_failed").All()
	if len(entries) != 1 {
		t.Fatalf("expected one audio diagnostic log, got %d", len(entries))
	}
	detail, ok := entries[0].ContextMap()["upstream_detail"].(string)
	if !ok {
		t.Fatalf("upstream diagnostic field = %#v, want string", entries[0].ContextMap()["upstream_detail"])
	}
	for _, secret := range []string{"https://provider.example", "secret", "bearer-secret", "task-secret", `C:\internal\audio.json`} {
		if strings.Contains(detail, secret) {
			t.Fatalf("diagnostic contains sensitive value %q: %q", secret, detail)
		}
	}
	if !strings.Contains(detail, "duration exceeds limit") {
		t.Fatalf("diagnostic lost useful failure reason: %q", detail)
	}
	if len([]rune(detail)) > 255 {
		t.Fatalf("diagnostic length = %d, want at most 255", len([]rune(detail)))
	}
	if len(repo.stateUpdates) == 0 || repo.stateUpdates[len(repo.stateUpdates)-1].ErrorMessage != HumanizeFileProcessingError("audio", "transcription_failed", "") {
		t.Fatal("user-facing audio failure message was not kept as the fixed safe message")
	}
}

func TestSanitizeAudioTaskFailureDiagnosticRedactsStructuredSecrets(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      []string
		forbidden []string
	}{
		{
			name:      "json credentials",
			input:     `provider response {"token":"task-secret","apiKey":"api-secret","client_secret":"client-secret-value"}; duration exceeds limit`,
			want:      []string{"token=[REDACTED]", "apiKey=[REDACTED]", "duration exceeds limit"},
			forbidden: []string{"task-secret", "api-secret", "client-secret-value"},
		},
		{
			name:      "escaped json credentials",
			input:     `provider response {\"token\":\"task-secret\",\"x-api-key\":\"api-secret\"}`,
			want:      []string{"token=[REDACTED]", "x-api-key=[REDACTED]"},
			forbidden: []string{"task-secret", "api-secret"},
		},
		{
			name:      "authorization and paths",
			input:     `Authorization: Basic basic-secret; x-api-key=api-secret; path=C:\internal\audio.json; unix=/var/lib/deeix/audio.json; duration 15/30 seconds`,
			want:      []string{"Authorization=[REDACTED]", "x-api-key=[REDACTED]", "[REDACTED_PATH]", "15/30 seconds"},
			forbidden: []string{"basic-secret", "api-secret", `C:\internal\audio.json`, "/var/lib/deeix/audio.json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sanitizeAudioTaskFailureDiagnostic(test.input)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("sanitized diagnostic = %q, want substring %q", got, want)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(got, forbidden) {
					t.Fatalf("sanitized diagnostic = %q, contains forbidden value %q", got, forbidden)
				}
			}
		})
	}
}

func timeNowForProcessingTest() (value time.Time) {
	return time.Now().Add(-time.Minute)
}
