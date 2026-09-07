// Package funasr implements a minimal DashScope Fun-ASR async client.
package funasr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	portfunasr "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/ports/funasr"
	sharedsecurity "github.com/DEEIX-AI/DEEIX-Chat/backend/internal/shared/security"
)

const (
	DefaultBaseURL = "https://dashscope.aliyuncs.com/api/v1"
	DefaultModel   = portfunasr.DefaultModel
)

var (
	ErrNotConfigured = portfunasr.ErrNotConfigured
	ErrTaskFailed    = errors.New("fun-asr task failed")
	ErrEmptyResult   = portfunasr.ErrEmptyResult
)

type Config struct {
	APIKey         string
	BaseURL        string
	Model          string
	HTTPClient     *http.Client
	OutboundPolicy func(string) error
}

type Client struct {
	apiKey         string
	baseURL        string
	model          string
	httpClient     *http.Client
	outboundPolicy func(string) error
}

func New(cfg Config) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultModel
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = sharedsecurity.NewOutboundHTTPClient(sharedsecurity.NewStrictOutboundPolicy(false), 120*time.Second)
	}
	return &Client{
		apiKey:         strings.TrimSpace(cfg.APIKey),
		baseURL:        base,
		model:          model,
		httpClient:     hc,
		outboundPolicy: cfg.OutboundPolicy,
	}
}

func (c *Client) Configured() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

func (c *Client) Model() string {
	if c == nil || strings.TrimSpace(c.model) == "" {
		return DefaultModel
	}
	return c.model
}

type SubmitInput = portfunasr.SubmitInput
type SubmitResult = portfunasr.SubmitResult
type TaskStatus = portfunasr.TaskStatus

var _ portfunasr.Client = (*Client)(nil)

type submitRequest struct {
	Model      string         `json:"model"`
	Input      map[string]any `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

// Submit starts an asynchronous Fun-ASR task with automatic speaker estimation.
func (c *Client) Submit(ctx context.Context, in SubmitInput) (*SubmitResult, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	fileURL := strings.TrimSpace(in.FileURL)
	parsedFileURL, err := url.Parse(fileURL)
	if err != nil || parsedFileURL == nil || parsedFileURL.Scheme != "https" || parsedFileURL.Host == "" || parsedFileURL.User != nil {
		return nil, fmt.Errorf("fun-asr: file url must be public https")
	}
	body := submitRequest{
		Model: c.model,
		Input: map[string]any{"file_urls": []string{fileURL}},
		Parameters: map[string]any{
			"diarization_enabled": true,
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/services/audio/asr/transcription", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-DashScope-Async", "enable")
	respBody, statusCode, err := c.do(req, 2<<20)
	if err != nil {
		return nil, err
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("fun-asr submit http %d: %s", statusCode, truncate(string(respBody), 500))
	}
	var envelope struct {
		RequestID string `json:"request_id"`
		Output    struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("fun-asr submit decode: %w", err)
	}
	if strings.TrimSpace(envelope.Output.TaskID) == "" {
		if envelope.Code != "" || envelope.Message != "" {
			return nil, fmt.Errorf("fun-asr submit failed: %s %s", envelope.Code, envelope.Message)
		}
		return nil, fmt.Errorf("fun-asr submit missing task_id")
	}
	return &SubmitResult{TaskID: envelope.Output.TaskID, RequestID: envelope.RequestID, Raw: respBody}, nil
}

func (c *Client) GetTask(ctx context.Context, taskID string) (*TaskStatus, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("fun-asr: empty task id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/tasks/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	respBody, statusCode, err := c.do(req, 2<<20)
	if err != nil {
		return nil, err
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("fun-asr task http %d: %s", statusCode, truncate(string(respBody), 500))
	}
	var envelope struct {
		Output struct {
			TaskID     string               `json:"task_id"`
			TaskStatus string               `json:"task_status"`
			Message    string               `json:"message"`
			Results    []taskResultEnvelope `json:"results"`
		} `json:"output"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("fun-asr task decode: %w", err)
	}
	status := strings.ToUpper(strings.TrimSpace(envelope.Output.TaskStatus))
	if status == "" && strings.TrimSpace(envelope.Code) != "" {
		return nil, fmt.Errorf("fun-asr task failed: %s %s", envelope.Code, envelope.Message)
	}
	out := &TaskStatus{
		TaskID:    firstNonEmpty(envelope.Output.TaskID, taskID),
		Status:    status,
		Message:   firstNonEmpty(envelope.Output.Message, envelope.Message),
		Raw:       respBody,
		Terminal:  isTerminal(status),
		Succeeded: status == "SUCCEEDED",
	}
	if out.Succeeded {
		for _, item := range envelope.Output.Results {
			if strings.EqualFold(item.SubtaskStatus, "FAILED") || strings.EqualFold(item.Output.SubtaskStatus, "FAILED") {
				out.Succeeded = false
				out.Message = firstNonEmpty(item.Message, item.Output.Message, out.Message, "subtask failed")
				break
			}
			if u := firstNonEmpty(item.TranscriptionURL, item.Output.TranscriptionURL); u != "" {
				out.TranscriptionURL = u
				break
			}
		}
		if out.Succeeded && out.TranscriptionURL == "" {
			out.Succeeded = false
			out.Message = "missing transcription_url"
		}
	}
	if out.Terminal && !out.Succeeded && out.Message == "" {
		out.Message = firstNonEmpty(envelope.Code, status, "task failed")
	}
	return out, nil
}

func (c *Client) DownloadJSON(ctx context.Context, rawURL string) (json.RawMessage, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("fun-asr: result url must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	respBody, statusCode, err := c.do(req, 32<<20)
	if err != nil {
		return nil, err
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("fun-asr download http %d: %s", statusCode, truncate(string(respBody), 300))
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil, ErrEmptyResult
	}
	return json.RawMessage(respBody), nil
}

func (c *Client) do(req *http.Request, maxBytes int64) ([]byte, int, error) {
	if c == nil || c.httpClient == nil {
		return nil, 0, ErrNotConfigured
	}
	if c.outboundPolicy != nil {
		if err := c.outboundPolicy(req.URL.String()); err != nil {
			return nil, 0, fmt.Errorf("fun-asr outbound url rejected: %w", err)
		}
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) > maxBytes {
		return nil, resp.StatusCode, fmt.Errorf("fun-asr response too large")
	}
	return body, resp.StatusCode, nil
}

type taskResultEnvelope struct {
	SubtaskStatus    string `json:"subtask_status"`
	TranscriptionURL string `json:"transcription_url"`
	Message          string `json:"message"`
	Output           struct {
		SubtaskStatus    string `json:"subtask_status"`
		TranscriptionURL string `json:"transcription_url"`
		Message          string `json:"message"`
	} `json:"output"`
}

func isTerminal(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCEEDED", "FAILED", "CANCELED", "UNKNOWN":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
