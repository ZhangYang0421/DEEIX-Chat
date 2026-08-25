package ocr

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTraditionalOCRPayloadPagesCount(t *testing.T) {
	var payload traditionalOCRPayload
	if err := json.Unmarshal([]byte(`{"pages":3,"text":"hello"}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if got := payload.PageCount(); got != 3 {
		t.Fatalf("PageCount() = %d, want 3", got)
	}
	if got := payload.ExtractedText(); got != "hello" {
		t.Fatalf("ExtractedText() = %q, want hello", got)
	}
}

func TestTraditionalOCRPayloadPagesItems(t *testing.T) {
	raw := `{
		"pages": [
			{"page": 1, "text": "first"},
			{"page_number": 2, "markdown": "second"}
		]
	}`
	var payload traditionalOCRPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if got := payload.PageCount(); got != 2 {
		t.Fatalf("PageCount() = %d, want 2", got)
	}

	pageTexts := payload.ExtractedPageTexts()
	if len(pageTexts) != 2 {
		t.Fatalf("len(ExtractedPageTexts()) = %d, want 2", len(pageTexts))
	}
	if pageTexts[0].PageNumber != 1 || pageTexts[0].Text != "first" {
		t.Fatalf("pageTexts[0] = %+v, want page 1 first", pageTexts[0])
	}
	if pageTexts[1].PageNumber != 2 || pageTexts[1].Text != "second" {
		t.Fatalf("pageTexts[1] = %+v, want page 2 second", pageTexts[1])
	}
}

func TestParsePaddleJobResponse(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"data": {
				"jobId": "job_123",
				"state": "done",
				"resultUrl": {"jsonUrl": "https://example.com/result.jsonl"}
			}
		}`)),
	}

	result, err := parsePaddleJobResponse(response, "poll")
	if err != nil {
		t.Fatalf("parsePaddleJobResponse() error = %v", err)
	}
	if result.JobID != "job_123" || result.State != "done" {
		t.Fatalf("parsePaddleJobResponse() = %+v", result)
	}
	if result.ResultURL != "https://example.com/result.jsonl" {
		t.Fatalf("ResultURL = %q", result.ResultURL)
	}
}

func TestParsePaddleJobResponseQueueFullIsRetryable(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"code":10010,"msg":"任务提交队列已满，请稍后重试"}`)),
	}

	_, err := parsePaddleJobResponse(response, "submit")
	if err == nil || !isPaddleRetryableError(err) {
		t.Fatalf("parsePaddleJobResponse() error = %v, want retryable", err)
	}
	if !strings.Contains(err.Error(), "10010") || !strings.Contains(err.Error(), "队列已满") {
		t.Fatalf("retryable error = %q", err.Error())
	}
}

func TestParsePaddleJobResponseBadRequestIsNotRetryable(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"code":10011,"msg":"invalid request"}`)),
	}

	_, err := parsePaddleJobResponse(response, "submit")
	if err == nil || isPaddleRetryableError(err) {
		t.Fatalf("parsePaddleJobResponse() error = %v, want non-retryable", err)
	}
}

func TestParsePaddleJobResponseServerErrorIsRetryable(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       io.NopCloser(strings.NewReader(`{"code":10010,"msg":"busy"}`)),
	}

	_, err := parsePaddleJobResponse(response, "submit")
	if err == nil || !isPaddleRetryableError(err) {
		t.Fatalf("parsePaddleJobResponse() error = %v, want retryable", err)
	}
}

func TestParsePaddleJobResponseEnvelopeUnauthorized(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"code":401,"msg":"Unauthorized"}`)),
	}

	_, err := parsePaddleJobResponse(response, "submit")
	if err == nil || err.Error() != "ocr_unauthorized" {
		t.Fatalf("parsePaddleJobResponse() error = %v, want ocr_unauthorized", err)
	}
}

func TestParsePaddleJSONL(t *testing.T) {
	raw := strings.Join([]string{
		`{"result":{"layoutParsingResults":[{"markdown":{"text":"第一页正文"}}]}}`,
		`{"result":{"layoutParsingResults":[{"markdown":{"text":"第二页正文"}}]}}`,
	}, "\n")

	result, err := parsePaddleJSONL(bytes.NewBufferString(raw), []PageRange{{Start: 2, End: 2}})
	if err != nil {
		t.Fatalf("parsePaddleJSONL() error = %v", err)
	}
	if result.RenderedPages != 2 {
		t.Fatalf("RenderedPages = %d, want 2", result.RenderedPages)
	}
	if len(result.Pages) != 1 || result.Pages[0].PageNumber != 2 || result.Pages[0].Text != "第二页正文" {
		t.Fatalf("Pages = %+v", result.Pages)
	}
	if result.Text != "第二页正文" {
		t.Fatalf("Text = %q", result.Text)
	}
}
