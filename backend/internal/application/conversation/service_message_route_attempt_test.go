package conversation

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/application/channel"
	model "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/domain/conversation"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/config"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/llm"
	"github.com/DEEIX-AI/DEEIX-Chat/backend/internal/infra/objectstore"
)

func TestBuildMessageRoutePromptRebuildsRouteSpecificFields(t *testing.T) {
	service := &Service{cfg: config.NewRuntime(config.Config{})}
	domainMessages := []model.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer", ReasoningContent: "private reasoning"},
		{Role: "user", Content: "follow up"},
	}
	baseInput := messageRoutePromptInput{
		UserContent:         "follow up",
		DomainMessages:      domainMessages,
		ProjectSystemPrompt: "project policy",
		Config: config.Config{
			DefaultSystemPrompt: "platform policy",
		},
	}

	chatInput := baseInput
	chatInput.ReasoningContentPassback = true
	chatPlan, err := service.buildMessageRoutePrompt(t.Context(), &channel.ResolvedRoute{
		Protocol:      llm.AdapterOpenAIChatCompletions,
		UpstreamModel: "deepseek-chat",
	}, chatInput)
	if err != nil {
		t.Fatalf("build chat prompt: %v", err)
	}
	if len(chatPlan.Messages) < 4 || chatPlan.Messages[0].Role != "system" {
		t.Fatalf("expected native system prompt, got %#v", chatPlan.Messages)
	}
	if chatPlan.Messages[2].ReasoningContent != "private reasoning" {
		t.Fatalf("expected reasoning passback, got %#v", chatPlan.Messages[2])
	}

	interactionInput := baseInput
	interactionPlan, err := service.buildMessageRoutePrompt(t.Context(), &channel.ResolvedRoute{
		Protocol:      llm.AdapterGeminiInteractions,
		UpstreamModel: "gemini-2.5-pro",
	}, interactionInput)
	if err != nil {
		t.Fatalf("build interaction prompt: %v", err)
	}
	for _, message := range interactionPlan.Messages {
		if message.Role == "system" {
			t.Fatalf("expected system prompt to be inlined, got %#v", interactionPlan.Messages)
		}
		if message.Role == "assistant" && message.ReasoningContent != "" {
			t.Fatalf("expected reasoning to be removed, got %#v", message)
		}
	}
	latest := interactionPlan.Messages[len(interactionPlan.Messages)-1]
	if latest.Role != "user" || !strings.Contains(latest.Content, "platform policy") || !strings.Contains(latest.Content, "follow up") {
		t.Fatalf("expected inlined system prompt on latest user message, got %#v", latest)
	}
}

func TestBuildMessageRoutePromptRejectsHistoricalImagesForTextOnlyModel(t *testing.T) {
	store := objectstore.NewLocal(t.TempDir())
	if _, err := store.Put(t.Context(), "images/one", bytes.NewReader([]byte("image-one")), objectstore.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatalf("put historical image: %v", err)
	}
	service := &Service{
		storeProvider:     &conversationTestStoreProvider{store: store},
		imageContextCache: defaultPreparedConversationImageCache(),
	}
	input := messageRoutePromptInput{
		UserContent: "继续分析",
		DomainMessages: []model.Message{
			{Role: "user", Content: "描述图片", Attachments: `[{"file_id":"image-1","kind":"image","mime_type":"image/png"}]`},
			{Role: "assistant", Content: "图片描述"},
			{Role: "user", Content: "继续分析"},
		},
		StableAttachments: []AttachmentInput{{
			FileID: "image-1", Kind: "image", MimeType: "image/png", StoragePath: "images/one", ContextMode: fileContextModeDirectImage,
		}},
		Config: config.Config{},
	}

	_, err := service.buildMessageRoutePrompt(t.Context(), &channel.ResolvedRoute{
		UpstreamModel:         "deepseek-chat",
		ModelCapabilitiesJSON: `{"inputModalities":["text"]}`,
	}, input)
	if !errors.Is(err, ErrModelImageInputUnsupported) {
		t.Fatalf("expected text-only route to reject historical image input, got %v", err)
	}
	if code := classifyRunErrorCode(err); code != MessageErrorCodeModelImageInputUnsupported {
		t.Fatalf("unexpected persisted image input error code: %q", code)
	}
}

func TestBuildMessageRoutePromptAllowsConfiguredImageInput(t *testing.T) {
	store := objectstore.NewLocal(t.TempDir())
	if _, err := store.Put(t.Context(), "images/one", bytes.NewReader([]byte("image-one")), objectstore.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatalf("put historical image: %v", err)
	}
	service := &Service{
		storeProvider:     &conversationTestStoreProvider{store: store},
		imageContextCache: defaultPreparedConversationImageCache(),
	}
	plan, err := service.buildMessageRoutePrompt(t.Context(), &channel.ResolvedRoute{
		UpstreamModel:         "vision-chat",
		ModelCapabilitiesJSON: `{"inputModalities":["text","image"]}`,
	}, messageRoutePromptInput{
		UserContent: "继续分析",
		DomainMessages: []model.Message{
			{Role: "user", Content: "描述图片", Attachments: `[{"file_id":"image-1","kind":"image","mime_type":"image/png"}]`},
			{Role: "user", Content: "继续分析"},
		},
		StableAttachments: []AttachmentInput{{
			FileID: "image-1", Kind: "image", MimeType: "image/png", StoragePath: "images/one", ContextMode: fileContextModeDirectImage,
		}},
		Config: config.Config{},
	})
	if err != nil {
		t.Fatalf("build image-capable prompt: %v", err)
	}
	if !promptMessagesContainImage(plan.Messages) {
		t.Fatalf("expected configured image input to remain in prompt: %#v", plan.Messages)
	}
}

func TestWithMessageRouteReasoningPassbackOptions(t *testing.T) {
	route := &channel.ResolvedRoute{
		ReasoningPassbackRequestOptions: map[string]interface{}{
			"preserve_thinking": true,
		},
	}
	messages := []llm.Message{{Role: "assistant", ReasoningContent: "historical reasoning"}}

	got := withMessageRouteReasoningPassbackOptions(nil, nil, route, true, messages)
	if got["preserve_thinking"] != true {
		t.Fatalf("expected fallback route reasoning option, got %#v", got)
	}

	explicit := withMessageRouteReasoningPassbackOptions(
		nil,
		map[string]interface{}{"preserve_thinking": false},
		route,
		true,
		messages,
	)
	if _, ok := explicit["preserve_thinking"]; ok {
		t.Fatalf("expected explicit option to prevent automatic override, got %#v", explicit)
	}

	withoutHistory := withMessageRouteReasoningPassbackOptions(nil, nil, route, true, []llm.Message{{Role: "user", Content: "hello"}})
	if _, ok := withoutHistory["preserve_thinking"]; ok {
		t.Fatalf("expected no option without historical reasoning, got %#v", withoutHistory)
	}
}
