package funasr

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Segment is one normalized transcript sentence.
type Segment struct {
	SegmentID     string  `json:"segmentID"`
	StartMs       int64   `json:"startMs"`
	EndMs         int64   `json:"endMs"`
	SpeakerID     *int    `json:"speakerID"`
	Text          string  `json:"text"`
	OriginalText  string  `json:"originalText"`
	AvgConfidence float64 `json:"avgConfidence,omitempty"`
	LowConfidence bool    `json:"lowConfidence,omitempty"`
	Edited        bool    `json:"edited,omitempty"`
}

// TranscriptDocument is the editable structured transcript stored as transcript.json.
type TranscriptDocument struct {
	Version          int               `json:"version"`
	Revision         int               `json:"revision"`
	Source           string            `json:"source"`
	Model            string            `json:"model"`
	FileID           string            `json:"fileID,omitempty"`
	FileName         string            `json:"fileName,omitempty"`
	DurationMs       *int64            `json:"durationMs,omitempty"`
	SpeakerNames     map[string]string `json:"speakerNames"`
	SpeakerOverrides map[string]string `json:"speakerOverrides,omitempty"`
	Segments         []Segment         `json:"segments"`
	UpdatedAt        string            `json:"updatedAt,omitempty"`
}

// NormalizeRaw converts a Fun-ASR result.raw.json payload into a TranscriptDocument.
func NormalizeRaw(raw json.RawMessage, model string) (*TranscriptDocument, error) {
	if len(bytesTrimSpace(raw)) == 0 {
		return nil, ErrEmptyResult
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("fun-asr normalize: %w", err)
	}
	if model == "" {
		model = DefaultModel
	}

	var durationMs *int64
	if props, ok := root["properties"].(map[string]any); ok {
		if v, ok := asInt64(props["original_duration_in_milliseconds"]); ok {
			durationMs = &v
		}
	}

	segments := make([]Segment, 0, 64)
	transcripts, _ := root["transcripts"].([]any)
	for _, t := range transcripts {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		sentences, _ := tm["sentences"].([]any)
		for _, s := range sentences {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			text := strings.TrimSpace(asString(sm["text"]))
			if text == "" {
				continue
			}
			startMs, _ := asInt64(sm["begin_time"])
			endMs, _ := asInt64(sm["end_time"])
			var speaker *int
			if sid, ok := asInt(sm["speaker_id"]); ok {
				speaker = &sid
			}
			sentenceID := asString(sm["sentence_id"])
			segID := sentenceID
			if segID == "" {
				segID = fmt.Sprintf("s_%d_%d", startMs, len(segments)+1)
			} else {
				segID = fmt.Sprintf("sentence_%s", segID)
			}
			avg, low := sentenceConfidence(sm)
			segments = append(segments, Segment{
				SegmentID:     segID,
				StartMs:       startMs,
				EndMs:         endMs,
				SpeakerID:     speaker,
				Text:          text,
				OriginalText:  text,
				AvgConfidence: avg,
				LowConfidence: low,
			})
		}
	}
	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].StartMs == segments[j].StartMs {
			return segments[i].EndMs < segments[j].EndMs
		}
		return segments[i].StartMs < segments[j].StartMs
	})
	if len(segments) == 0 {
		return nil, ErrEmptyResult
	}

	names := map[string]string{}
	for _, seg := range segments {
		if seg.SpeakerID == nil {
			continue
		}
		key := fmt.Sprintf("%d", *seg.SpeakerID)
		if _, ok := names[key]; !ok {
			names[key] = fmt.Sprintf("说话人%d", *seg.SpeakerID+1)
		}
	}

	return &TranscriptDocument{
		Version:      1,
		Revision:     1,
		Source:       "fun-asr",
		Model:        model,
		DurationMs:   durationMs,
		SpeakerNames: names,
		Segments:     segments,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// RenderMarkdown builds the RAG/preview markdown from a transcript document.
// When forRAG is true, content is grouped into ~2 minute windows with ~15s overlap.
func RenderMarkdown(doc *TranscriptDocument, forRAG bool) string {
	if doc == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# 录音转写\n\n")
	if doc.FileName != "" {
		b.WriteString(fmt.Sprintf("- 文件：`%s`\n", doc.FileName))
	}
	if doc.Model != "" {
		b.WriteString(fmt.Sprintf("- 模型：`%s`\n", doc.Model))
	}
	b.WriteString("- 说话人编号由模型自动生成，可在页面重命名或逐句修正归属。\n\n")
	if forRAG {
		b.WriteString("## 分片转写\n\n")
		for _, chunk := range BuildTimeWindowChunks(doc, 2*time.Minute, 15*time.Second) {
			b.WriteString(chunk)
			b.WriteString("\n\n")
		}
		return strings.TrimSpace(b.String()) + "\n"
	}
	b.WriteString("## 转写结果\n\n")
	for _, seg := range doc.Segments {
		speaker := speakerLabelForSegment(doc, seg)
		start := formatTimestamp(seg.StartMs)
		end := formatTimestamp(seg.EndMs)
		flag := ""
		if seg.LowConfidence {
			flag = " ⚠️建议试听"
		}
		b.WriteString(fmt.Sprintf("**[%s - %s] %s：** %s%s\n\n", start, end, speaker, seg.Text, flag))
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// BuildTimeWindowChunks groups segments into time windows for RAG embedding.
// window/overlap are durations; overlap is clamped below window.
func BuildTimeWindowChunks(doc *TranscriptDocument, window, overlap time.Duration) []string {
	if doc == nil || len(doc.Segments) == 0 {
		return nil
	}
	if window <= 0 {
		window = 2 * time.Minute
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= window {
		overlap = window / 8
	}
	windowMs := window.Milliseconds()
	overlapMs := overlap.Milliseconds()
	step := windowMs - overlapMs
	if step <= 0 {
		step = windowMs
	}

	// Determine end bound.
	var maxEnd int64
	for _, seg := range doc.Segments {
		if seg.EndMs > maxEnd {
			maxEnd = seg.EndMs
		}
	}
	if maxEnd <= 0 {
		maxEnd = doc.Segments[len(doc.Segments)-1].StartMs + 1
	}

	chunks := make([]string, 0, int(maxEnd/step)+1)
	for start := int64(0); start < maxEnd; start += step {
		end := start + windowMs
		var lines []string
		for _, seg := range doc.Segments {
			// include if overlaps [start, end)
			if seg.EndMs <= start || seg.StartMs >= end {
				continue
			}
			speaker := speakerLabelForSegment(doc, seg)
			lines = append(lines, fmt.Sprintf("%s：%s", speaker, seg.Text))
		}
		if len(lines) == 0 {
			// Skip empty leading/trailing windows; stop if past last content.
			if start > maxEnd {
				break
			}
			continue
		}
		header := fmt.Sprintf("[%s - %s]", formatTimestamp(start), formatTimestamp(minInt64(end, maxEnd)))
		chunks = append(chunks, header+"\n"+strings.Join(lines, "\n"))
		if end >= maxEnd {
			break
		}
	}
	if len(chunks) == 0 {
		// fallback single chunk
		return []string{strings.TrimSpace(RenderMarkdown(doc, false))}
	}
	return chunks
}

func speakerLabelForSegment(doc *TranscriptDocument, seg Segment) string {
	key := ""
	if doc != nil && doc.SpeakerOverrides != nil {
		key = strings.TrimSpace(doc.SpeakerOverrides[seg.SegmentID])
	}
	if key == "" && seg.SpeakerID != nil {
		key = fmt.Sprintf("%d", *seg.SpeakerID)
	}
	if key == "" {
		return "说话人未知"
	}
	if doc != nil && doc.SpeakerNames != nil {
		if name := strings.TrimSpace(doc.SpeakerNames[key]); name != "" {
			return name
		}
	}
	if speakerID, err := strconv.Atoi(key); err == nil {
		return fmt.Sprintf("说话人%d", speakerID+1)
	}
	return key
}

func formatTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms %= 3_600_000
	m := ms / 60_000
	ms %= 60_000
	s := ms / 1_000
	milli := ms % 1_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, milli)
}

func sentenceConfidence(sm map[string]any) (float64, bool) {
	words, _ := sm["words"].([]any)
	if len(words) == 0 {
		return 0, false
	}
	var sum float64
	var n int
	lowWord := false
	latinSuspect := false
	for _, w := range words {
		wm, ok := w.(map[string]any)
		if !ok {
			continue
		}
		if c, ok := asFloat64(wm["confidence"]); ok {
			sum += c
			n++
			if c < 0.3 {
				lowWord = true
			}
		}
		text := asString(wm["text"])
		if looksLatinToken(text) {
			latinSuspect = true
		}
	}
	if n == 0 {
		return 0, latinSuspect
	}
	avg := sum / float64(n)
	return avg, avg < 0.45 || lowWord || latinSuspect
}

func looksLatinToken(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	letters := 0
	latin := 0
	for _, r := range text {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			latin++
			letters++
		} else if r > 127 {
			letters++
		}
	}
	return letters > 0 && latin == letters && latin >= 3
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}
}

func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	case string:
		var i int64
		_, err := fmt.Sscan(t, &i)
		return i, err == nil
	default:
		return 0, false
	}
}

func asInt(v any) (int, bool) {
	i, ok := asInt64(v)
	return int(i), ok
}

func asFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
