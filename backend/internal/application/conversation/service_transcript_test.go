package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appstorage "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/application/objectstorage"
	domainconversation "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	portfunasr "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/funasr"
	portobjectstore "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/objectstore"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/repository"
)

const (
	transcriptTestFileID       = "transcript-file"
	transcriptTestJSONPath     = "transcripts/transcript.json"
	transcriptTestMarkdownPath = "transcripts/transcript.md"
	transcriptTestRawPath      = "transcripts/result.raw.json"
)

type transcriptPatchRepositoryStub struct {
	repository.ConversationRepository

	mu             sync.Mutex
	file           domainconversation.FileObject
	processing     domainconversation.FileObjectProcessing
	revision       int
	fileErr        error
	processingErr  error
	publishErr     error
	publishBarrier *sync.WaitGroup
	publishCalls   int
	published      []repository.PublishTranscriptRevisionInput
}

func (r *transcriptPatchRepositoryStub) GetActiveFileObjectByID(context.Context, uint, string) (*domainconversation.FileObject, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fileErr != nil {
		return nil, r.fileErr
	}
	file := r.file
	return &file, nil
}

func (r *transcriptPatchRepositoryStub) GetFileObjectProcessingByObjectID(context.Context, uint) (*domainconversation.FileObjectProcessing, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.processingErr != nil {
		return nil, r.processingErr
	}
	processing := r.processing
	return &processing, nil
}

func (r *transcriptPatchRepositoryStub) PublishTranscriptRevision(
	_ context.Context,
	_ uint,
	_ string,
	input repository.PublishTranscriptRevisionInput,
) (bool, error) {
	if r.publishBarrier != nil {
		r.publishBarrier.Done()
		r.publishBarrier.Wait()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publishCalls++
	r.published = append(r.published, input)
	if r.publishErr != nil {
		return false, r.publishErr
	}
	if r.revision != input.ExpectedRevision {
		return false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(r.processing.PayloadJSON), &payload); err != nil {
		return false, err
	}
	payload["transcriptRevision"] = input.Revision
	payload["transcriptJSONPath"] = input.TranscriptJSONPath
	payload["transcriptMDPath"] = input.TranscriptMDPath
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	r.revision = input.Revision
	r.processing.PayloadJSON = string(payloadJSON)
	r.processing.ExtractStoragePath = input.TranscriptMDPath
	r.processing.ExtractChars = input.ExtractChars
	r.processing.PreviewText = input.PreviewText
	r.file.ProcessingPayloadJSON = string(payloadJSON)
	r.file.ExtractStoragePath = input.TranscriptMDPath
	r.file.ExtractChars = input.ExtractChars
	r.file.PreviewText = input.PreviewText
	r.file.RAGReady = false
	if r.file.EmbedStatus == "none" {
		r.file.RAGReason = "embedding_pending"
	} else {
		r.file.EmbedStatus = "stale"
		r.file.RAGReason = "embedding_stale"
	}
	return true, nil
}

func (r *transcriptPatchRepositoryStub) snapshot() (int, int, string, []repository.PublishTranscriptRevisionInput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision, r.publishCalls, r.processing.PayloadJSON, append([]repository.PublishTranscriptRevisionInput(nil), r.published...)
}

type transcriptPatchStoreProvider struct {
	store portobjectstore.Store
}

func (p transcriptPatchStoreProvider) Open(context.Context) (portobjectstore.Store, error) {
	return p.store, nil
}

var _ appstorage.Provider = transcriptPatchStoreProvider{}

type transcriptPatchStore struct {
	mu           sync.Mutex
	objects      map[string][]byte
	contentTypes map[string]string
	failPut      int
	putCount     int
	putAttempts  []string
	deletes      []string
	deleteErr    error
}

func (s *transcriptPatchStore) Put(_ context.Context, key string, body io.Reader, opts portobjectstore.PutOptions) (portobjectstore.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return portobjectstore.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCount++
	s.putAttempts = append(s.putAttempts, key)
	if s.failPut == s.putCount {
		return portobjectstore.ObjectInfo{}, errors.New("injected transcript object write failure")
	}
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	if s.contentTypes == nil {
		s.contentTypes = make(map[string]string)
	}
	s.objects[key] = append([]byte(nil), data...)
	s.contentTypes[key] = opts.ContentType
	return portobjectstore.ObjectInfo{Key: key, SizeBytes: int64(len(data)), ContentType: opts.ContentType}, nil
}

func (s *transcriptPatchStore) Open(_ context.Context, key string) (io.ReadCloser, portobjectstore.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, portobjectstore.ObjectInfo{}, portobjectstore.ErrNotFound
	}
	copyData := append([]byte(nil), data...)
	return io.NopCloser(bytes.NewReader(copyData)), portobjectstore.ObjectInfo{
		Key:         key,
		SizeBytes:   int64(len(copyData)),
		ContentType: s.contentTypes[key],
	}, nil
}

func (s *transcriptPatchStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, key)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if _, exists := s.objects[key]; !exists {
		return portobjectstore.ErrNotFound
	}
	delete(s.objects, key)
	delete(s.contentTypes, key)
	return nil
}

func (s *transcriptPatchStore) Materialize(context.Context, string) (string, func(), error) {
	return "", nil, portobjectstore.ErrUnsupported
}

func (s *transcriptPatchStore) snapshot() (map[string][]byte, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make(map[string][]byte, len(s.objects))
	for key, value := range s.objects {
		objects[key] = append([]byte(nil), value...)
	}
	return objects, append([]string(nil), s.putAttempts...), append([]string(nil), s.deletes...)
}

var _ portobjectstore.Store = (*transcriptPatchStore)(nil)

type transcriptPatchCodec struct{}

func (transcriptPatchCodec) Normalize(json.RawMessage, string) (*portfunasr.TranscriptDocument, error) {
	return nil, errors.New("not implemented in transcript patch test codec")
}

func (transcriptPatchCodec) RenderMarkdown(*portfunasr.TranscriptDocument, bool) string {
	return "new markdown"
}

func (transcriptPatchCodec) BuildTimeWindowChunks(*portfunasr.TranscriptDocument, time.Duration, time.Duration) []string {
	return nil
}

var _ portfunasr.Codec = transcriptPatchCodec{}

type transcriptPatchFixture struct {
	service          *Service
	repo             *transcriptPatchRepositoryStub
	store            *transcriptPatchStore
	originalJSON     []byte
	originalMarkdown []byte
	rawSentinel      []byte
}

func newTranscriptPatchFixture() transcriptPatchFixture {
	originalJSON := []byte(`{
  "version": 1,
  "revision": 1,
  "source": "fun-asr",
  "model": "fun-asr",
  "fileID": "transcript-file",
  "fileName": "meeting.mp3",
  "speakerNames": {"1": "Speaker 1"},
  "segments": [{"segmentID": "segment-1", "speakerID": 1, "text": "old text", "originalText": "old text"}]
}`)
	originalMarkdown := []byte("old markdown")
	rawSentinel := []byte(`{"immutable":"raw-sentinel","bytes":[0,1,2,255]}`)
	payload := `{"rawResultPath":"` + transcriptTestRawPath + `","transcriptJSONPath":"` + transcriptTestJSONPath + `","transcriptMDPath":"` + transcriptTestMarkdownPath + `","transcriptRevision":1}`
	repo := &transcriptPatchRepositoryStub{
		file: domainconversation.FileObject{
			ID:                    7,
			FileID:                transcriptTestFileID,
			UserID:                11,
			FileCategory:          "audio",
			Status:                "active",
			StoragePath:           "uploads/meeting.mp3",
			ExtractStatus:         "ready",
			ExtractStoragePath:    transcriptTestMarkdownPath,
			ProcessingStatus:      "ready",
			ProcessingReady:       true,
			ProcessingPayloadJSON: payload,
			EmbedStatus:           "ready",
			RAGReady:              true,
			RAGReason:             "ready",
		},
		processing: domainconversation.FileObjectProcessing{
			FileObjectID:       7,
			UserID:             11,
			FileCategory:       "audio",
			ExtractStoragePath: transcriptTestMarkdownPath,
			PayloadJSON:        payload,
		},
		revision: 1,
	}
	store := &transcriptPatchStore{
		objects: map[string][]byte{
			transcriptTestJSONPath:     append([]byte(nil), originalJSON...),
			transcriptTestMarkdownPath: append([]byte(nil), originalMarkdown...),
			transcriptTestRawPath:      append([]byte(nil), rawSentinel...),
		},
		contentTypes: map[string]string{
			transcriptTestJSONPath:     "application/json",
			transcriptTestMarkdownPath: "text/markdown; charset=utf-8",
			transcriptTestRawPath:      "application/json",
		},
	}
	service := &Service{
		repo:          repo,
		storeProvider: transcriptPatchStoreProvider{store: store},
		funASRCodec:   transcriptPatchCodec{},
	}
	return transcriptPatchFixture{
		service:          service,
		repo:             repo,
		store:            store,
		originalJSON:     originalJSON,
		originalMarkdown: originalMarkdown,
		rawSentinel:      rawSentinel,
	}
}

func transcriptTestPatch(text string) TranscriptPatch {
	return TranscriptPatch{
		Revision: 1,
		Segments: []TranscriptSegmentPatch{{SegmentID: "segment-1", Text: text}},
	}
}

func assertTranscriptSentinelsUnchanged(t *testing.T, fixture transcriptPatchFixture) {
	t.Helper()
	objects, _, _ := fixture.store.snapshot()
	if !bytes.Equal(objects[transcriptTestJSONPath], fixture.originalJSON) {
		t.Fatalf("published JSON object changed in place: %q", objects[transcriptTestJSONPath])
	}
	if !bytes.Equal(objects[transcriptTestMarkdownPath], fixture.originalMarkdown) {
		t.Fatalf("published Markdown object changed in place: %q", objects[transcriptTestMarkdownPath])
	}
	beforeHash := sha256.Sum256(fixture.rawSentinel)
	afterHash := sha256.Sum256(objects[transcriptTestRawPath])
	if beforeHash != afterHash || !bytes.Equal(objects[transcriptTestRawPath], fixture.rawSentinel) {
		t.Fatal("raw transcript sentinel changed")
	}
}

func assertCandidatePath(t *testing.T, path, suffix string) {
	t.Helper()
	prefix := ".transcripts/uid_11/" + transcriptTestFileID + "/revisions/rev-2-"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		t.Fatalf("candidate path = %q, want prefix %q and suffix %q", path, prefix, suffix)
	}
	if strings.Contains(path, "\\") || strings.Contains(path, ".tmp") {
		t.Fatalf("candidate path is not normalized immutable path: %q", path)
	}
}

func TestPatchFileTranscriptPublishesUniqueImmutableArtifacts(t *testing.T) {
	fixture := newTranscriptPatchFixture()

	result, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("new text"))
	if err != nil {
		t.Fatalf("PatchFileTranscript() error = %v", err)
	}
	if result == nil || result.Document.Revision != 2 {
		t.Fatalf("patched document = %#v, want revision 2", result)
	}
	assertCandidatePath(t, result.JSONPath, "/transcript.json")
	assertCandidatePath(t, result.MarkdownPath, "/transcript.md")
	if filepath.Dir(result.JSONPath) != filepath.Dir(result.MarkdownPath) {
		t.Fatalf("candidate directories differ: %q, %q", result.JSONPath, result.MarkdownPath)
	}

	revision, publishCalls, payloadJSON, published := fixture.repo.snapshot()
	if revision != 2 || publishCalls != 1 || len(published) != 1 {
		t.Fatalf("publication state = revision %d, calls %d, inputs %#v", revision, publishCalls, published)
	}
	var payload processingTranscriptPayload
	if err = json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("decode published payload: %v", err)
	}
	if payload.TranscriptRevision != 2 || payload.TranscriptJSONPath != result.JSONPath || payload.TranscriptMDPath != result.MarkdownPath {
		t.Fatalf("published payload = %#v", payload)
	}
	if payload.RawResultPath != transcriptTestRawPath || result.RawPath != transcriptTestRawPath {
		t.Fatalf("raw path changed: payload=%q result=%q", payload.RawResultPath, result.RawPath)
	}
	objects, puts, deletes := fixture.store.snapshot()
	if len(puts) != 2 || puts[0] != result.JSONPath || puts[1] != result.MarkdownPath {
		t.Fatalf("put attempts = %#v, want one JSON then one Markdown", puts)
	}
	if len(deletes) != 0 {
		t.Fatalf("successful patch deleted objects: %#v", deletes)
	}
	if string(objects[result.MarkdownPath]) != "new markdown" {
		t.Fatalf("new Markdown artifact = %q", objects[result.MarkdownPath])
	}
	for _, key := range puts {
		if key == transcriptTestRawPath || strings.Contains(key, ".tmp") {
			t.Fatalf("unexpected write target %q", key)
		}
	}
	assertTranscriptSentinelsUnchanged(t, fixture)
}

func TestPatchFileTranscriptConcurrentCASKeepsWinnerArtifacts(t *testing.T) {
	fixture := newTranscriptPatchFixture()
	barrier := &sync.WaitGroup{}
	barrier.Add(2)
	fixture.repo.publishBarrier = barrier

	type outcome struct {
		result *TranscriptResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, text := range []string{"first concurrent edit", "second concurrent edit"} {
		text := text
		go func() {
			result, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch(text))
			outcomes <- outcome{result: result, err: err}
		}()
	}
	first, second := <-outcomes, <-outcomes
	close(outcomes)

	var winner *TranscriptResult
	conflicts := 0
	for _, item := range []outcome{first, second} {
		switch {
		case item.err == nil:
			if winner != nil {
				t.Fatal("both concurrent patches succeeded")
			}
			winner = item.result
		case errors.Is(item.err, ErrTranscriptRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent patch error = %v", item.err)
		}
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("winner=%#v conflicts=%d, want one of each", winner, conflicts)
	}

	objects, puts, deletes := fixture.store.snapshot()
	if len(puts) != 4 {
		t.Fatalf("put attempts = %#v, want four unique candidate writes", puts)
	}
	seen := make(map[string]int, len(puts))
	roots := make(map[string]struct{}, 2)
	for _, key := range puts {
		seen[key]++
		roots[filepath.Dir(key)] = struct{}{}
	}
	if len(seen) != 4 || len(roots) != 2 {
		t.Fatalf("candidate paths are not unique: puts=%#v", puts)
	}
	if len(deletes) != 2 {
		t.Fatalf("loser cleanup deletes = %#v, want two", deletes)
	}
	for _, key := range deletes {
		if key == winner.JSONPath || key == winner.MarkdownPath {
			t.Fatalf("loser cleanup deleted winner artifact %q", key)
		}
		if _, exists := objects[key]; exists {
			t.Fatalf("loser candidate still exists after cleanup: %q", key)
		}
	}
	if _, exists := objects[winner.JSONPath]; !exists {
		t.Fatalf("winner JSON artifact missing: %q", winner.JSONPath)
	}
	if _, exists := objects[winner.MarkdownPath]; !exists {
		t.Fatalf("winner Markdown artifact missing: %q", winner.MarkdownPath)
	}
	revision, publishCalls, payloadJSON, _ := fixture.repo.snapshot()
	if revision != 2 || publishCalls != 2 {
		t.Fatalf("repository revision/calls = %d/%d, want 2/2", revision, publishCalls)
	}
	if !strings.Contains(payloadJSON, winner.JSONPath) || !strings.Contains(payloadJSON, winner.MarkdownPath) {
		t.Fatalf("payload does not point to winner: %s", payloadJSON)
	}
	assertTranscriptSentinelsUnchanged(t, fixture)
}

func TestPatchFileTranscriptSharedClonePublishesIntoCurrentNamespace(t *testing.T) {
	fixture := newTranscriptPatchFixture()
	const (
		sharedJSON = ".transcripts/uid_99/source-file/transcript.json"
		sharedMD   = ".transcripts/uid_99/source-file/transcript.md"
		sharedRaw  = ".transcripts/uid_99/source-file/result.raw.json"
	)
	fixture.store.mu.Lock()
	fixture.store.objects = map[string][]byte{
		sharedJSON: append([]byte(nil), fixture.originalJSON...),
		sharedMD:   append([]byte(nil), fixture.originalMarkdown...),
		sharedRaw:  append([]byte(nil), fixture.rawSentinel...),
	}
	fixture.store.contentTypes = map[string]string{
		sharedJSON: "application/json",
		sharedMD:   "text/markdown; charset=utf-8",
		sharedRaw:  "application/json",
	}
	fixture.store.mu.Unlock()
	payload := `{"rawResultPath":"` + sharedRaw + `","transcriptJSONPath":"` + sharedJSON + `","transcriptMDPath":"` + sharedMD + `"}`
	fixture.repo.mu.Lock()
	fixture.repo.processing.PayloadJSON = payload
	fixture.repo.file.ProcessingPayloadJSON = payload
	fixture.repo.mu.Unlock()

	result, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("clone edit"))
	if err != nil {
		t.Fatalf("PatchFileTranscript() error = %v", err)
	}
	assertCandidatePath(t, result.JSONPath, "/transcript.json")
	assertCandidatePath(t, result.MarkdownPath, "/transcript.md")
	objects, puts, _ := fixture.store.snapshot()
	if !bytes.Equal(objects[sharedJSON], fixture.originalJSON) || !bytes.Equal(objects[sharedMD], fixture.originalMarkdown) {
		t.Fatal("shared source transcript artifacts changed")
	}
	if sha256.Sum256(objects[sharedRaw]) != sha256.Sum256(fixture.rawSentinel) {
		t.Fatal("shared source raw artifact changed")
	}
	for _, key := range puts {
		if strings.HasPrefix(key, ".transcripts/uid_99/") {
			t.Fatalf("patch wrote into source owner namespace: %q", key)
		}
	}
}

func TestPatchFileTranscriptPutFailuresCleanOnlyCandidates(t *testing.T) {
	for _, test := range []struct {
		name    string
		failPut int
		puts    int
	}{
		{name: "json", failPut: 1, puts: 1},
		{name: "markdown", failPut: 2, puts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTranscriptPatchFixture()
			fixture.store.failPut = test.failPut

			_, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("failed edit"))
			if err == nil || !strings.Contains(err.Error(), "injected transcript object write failure") {
				t.Fatalf("PatchFileTranscript() error = %v, want injected write failure", err)
			}
			revision, publishCalls, _, _ := fixture.repo.snapshot()
			if revision != 1 || publishCalls != 0 {
				t.Fatalf("repository revision/calls = %d/%d, want 1/0", revision, publishCalls)
			}
			objects, puts, deletes := fixture.store.snapshot()
			if len(puts) != test.puts || len(deletes) != 2 {
				t.Fatalf("put/delete attempts = %#v/%#v", puts, deletes)
			}
			for _, key := range deletes {
				if _, exists := objects[key]; exists {
					t.Fatalf("candidate survived failed write cleanup: %q", key)
				}
			}
			assertTranscriptSentinelsUnchanged(t, fixture)
		})
	}
}

func TestPatchFileTranscriptPublishErrorCleansCandidates(t *testing.T) {
	fixture := newTranscriptPatchFixture()
	publishErr := errors.New("database unavailable")
	fixture.repo.publishErr = publishErr

	_, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("db failure"))
	if !errors.Is(err, publishErr) {
		t.Fatalf("PatchFileTranscript() error = %v, want publish error", err)
	}
	revision, publishCalls, _, _ := fixture.repo.snapshot()
	if revision != 1 || publishCalls != 1 {
		t.Fatalf("repository revision/calls = %d/%d, want 1/1", revision, publishCalls)
	}
	objects, _, deletes := fixture.store.snapshot()
	if len(deletes) != 2 {
		t.Fatalf("publish error deletes = %#v, want two", deletes)
	}
	for _, key := range deletes {
		if _, exists := objects[key]; exists {
			t.Fatalf("candidate survived publish error cleanup: %q", key)
		}
	}
	assertTranscriptSentinelsUnchanged(t, fixture)
}

func TestPatchFileTranscriptCASConflictCleansCandidates(t *testing.T) {
	fixture := newTranscriptPatchFixture()
	fixture.repo.revision = 2

	_, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("stale edit"))
	if !errors.Is(err, ErrTranscriptRevisionConflict) {
		t.Fatalf("PatchFileTranscript() error = %v, want revision conflict", err)
	}
	revision, publishCalls, _, _ := fixture.repo.snapshot()
	if revision != 2 || publishCalls != 1 {
		t.Fatalf("repository revision/calls = %d/%d, want 2/1", revision, publishCalls)
	}
	objects, _, deletes := fixture.store.snapshot()
	if len(deletes) != 2 {
		t.Fatalf("conflict cleanup deletes = %#v, want two", deletes)
	}
	for _, key := range deletes {
		if _, exists := objects[key]; exists {
			t.Fatalf("conflicting candidate survived cleanup: %q", key)
		}
	}
	assertTranscriptSentinelsUnchanged(t, fixture)
}

func TestPatchFileTranscriptConflictRemainsDetectableWhenCleanupFails(t *testing.T) {
	fixture := newTranscriptPatchFixture()
	fixture.repo.revision = 2
	fixture.store.deleteErr = errors.New("cleanup unavailable")

	_, err := fixture.service.PatchFileTranscript(context.Background(), 11, transcriptTestFileID, transcriptTestPatch("stale edit"))
	if !errors.Is(err, ErrTranscriptRevisionConflict) || !strings.Contains(err.Error(), "cleanup unavailable") {
		t.Fatalf("PatchFileTranscript() error = %v, want conflict joined with cleanup error", err)
	}
}

func TestGetFileTranscriptMapsRepositoryNotFoundSentinels(t *testing.T) {
	for _, sentinel := range []error{repository.ErrNotFound, repository.ErrFileNotFound} {
		fixture := newTranscriptPatchFixture()
		fixture.repo.fileErr = sentinel
		if _, err := fixture.service.GetFileTranscript(context.Background(), 11, transcriptTestFileID); !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("GetFileTranscript(%v) error = %v, want ErrFileNotFound", sentinel, err)
		}
	}
}

func TestGetFileTranscriptPreservesProcessingRepositoryErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "not_found", err: repository.ErrNotFound, want: ErrFileProcessingNotReady},
		{name: "file_not_found", err: repository.ErrFileNotFound, want: ErrFileProcessingNotReady},
		{name: "database", err: errors.New("database unavailable")},
		{name: "context", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTranscriptPatchFixture()
			fixture.repo.processingErr = test.err
			_, err := fixture.service.GetFileTranscript(context.Background(), 11, transcriptTestFileID)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("GetFileTranscript() error = %v, want %v", err, test.want)
				}
				return
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("GetFileTranscript() error = %v, want original %v", err, test.err)
			}
		})
	}
}
