package sessions

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type jsonlCountingReader struct {
	io.ReaderAt
	bytes int
}

func (r *jsonlCountingReader) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(buffer, offset)
	r.bytes += n
	return n, err
}

func TestReverseJSONLReadsCompleteRecordsAcrossBlocks(t *testing.T) {
	large := strings.Repeat("界", 6*1024)
	for _, test := range []struct {
		name, input string
		want        []string
	}{
		{name: "empty"},
		{name: "LF", input: "{\"text\":\"first\"}\n{\"text\":\"last\"}\n", want: []string{"last", "first"}},
		{name: "CRLF", input: "{\"text\":\"first\"}\r\n{\"text\":\"last\"}\r\n", want: []string{"last", "first"}},
		{name: "no final newline", input: `{"text":"last"}`, want: []string{"last"}},
		{name: "partial final record", input: "not JSON\n{\"text\":\"valid\"}\n{\"text\":", want: []string{"valid"}},
		{name: "large final record", input: "{\"text\":\"first\"}\n{\"text\":\"" + large + "\"}", want: []string{large, "first"}},
		{name: "large initial record", input: "{\"text\":\"" + large + "\"}\n{\"text\":\"last\"}\n", want: []string{"last", large}},
		{name: "large invalid line", input: "{\"text\":\"first\"}\n" + large + "\n{\"text\":\"last\"}\n", want: []string{"last", "first"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			err := scanJSONLReverseReader(strings.NewReader(test.input), int64(len(test.input)), func(line []byte) bool {
				var record map[string]any
				if json.Unmarshal(line, &record) == nil {
					if text, ok := record["text"].(string); ok {
						got = append(got, text)
					}
				}
				return false
			})
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("records = %q, want %q, error = %v", got, test.want, err)
			}
		})
	}
}

func TestReverseJSONLStopsWithoutReadingHistory(t *testing.T) {
	input := strings.Repeat("{\"text\":\"old\"}\n", 10000) + `{"text":"latest"}`
	reader := &jsonlCountingReader{ReaderAt: strings.NewReader(input)}
	visits := 0
	err := scanJSONLReverseReader(reader, int64(len(input)), func(line []byte) bool {
		visits++
		if string(line) != `{"text":"latest"}` {
			t.Fatalf("unexpected final record: %s", line)
		}
		return true
	})
	if err != nil || visits != 1 || reader.bytes > 4*1024 {
		t.Fatalf("visits = %d, bytes = %d, error = %v", visits, reader.bytes, err)
	}
}

func TestReverseJSONLReportsFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if err := scanJSONLReverse(missing, func([]byte) bool { return false }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error: %v", err)
	}
	if err := scanJSONLReverse(t.TempDir(), func([]byte) bool { return false }); err == nil {
		t.Fatal("directory read succeeded")
	}
}

func TestSessionTimestampUsesLargeFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFixture(t, path, "{\"timestamp\":\"2026-11-13T08:00:00Z\"}\n"+
		"{\"timestamp\":\"2026-11-13T08:05:00Z\",\"payload\":\""+strings.Repeat("x", 300*1024)+"\"}\n{\"timestamp\":")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got := sessionFileTimestamp(path); got != "2026-11-13T08:05:00Z" {
		t.Fatalf("last native timestamp = %q", got)
	}
}

func TestJSONLSummaryMatchesCanonicalMessages(t *testing.T) {
	for _, test := range []struct {
		agent   string
		records []string
	}{
		{agent: "claude", records: []string{
			`{"type":"user","sessionId":"s1","cwd":"/work","message":{"content":"First request"}}`,
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"thought"},{"type":"tool_use","id":"call","name":"run","input":{}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"not a user request"},{"type":"tool_result","tool_use_id":"call-2","content":"another output"},{"type":"text","text":"Latest request"}]}}`,
			`{"type":"system","content":"system context"}`,
			`{"type":"assistant","message":{"content":[]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"final tool output"}]}}`,
			`{"type":"user","message":{"content":"<environment_context>Injected"}}`,
		}},
		{agent: "codex", records: []string{
			`{"type":"session_meta","payload":{"id":"s1","cwd":"/work"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":"First request"}}`,
			`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}}`,
			`{"type":"response_item","payload":{"type":"function_call","call_id":"one","name":"run","arguments":"{}"}}`,
			`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"two","name":"patch","input":"raw"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"one","output":"result"}}`,
			`{"type":"response_item","payload":{"type":"reasoning","summary":[]}}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"answer"}}`,
			`{"type":"response_item","payload":{"type":"function_call","call_id":"three","name":"run","arguments":"{}"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"developer","content":"system"}}`,
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Latest request"}]}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":"# AGENTS.md instructions\nInjected"}}`,
		}},
		{agent: "pi", records: []string{
			`{"type":"session","id":"s1","cwd":"/work"}`,
			`{"type":"message","message":{"role":"user","content":"First request"}}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"thought"},{"type":"toolCall","id":"one","name":"run","arguments":{}}]}}`,
			`{"type":"message","message":{"role":"toolResult","toolCallId":"one","content":[]}}`,
			`{"type":"message","message":{"role":"system","sections":{"rules":"system"}}}`,
			`{"type":"message","message":{"role":"assistant","content":[]}}`,
			`{"type":"message","message":{"role":"user","content":"Latest request"}}`,
			`{"type":"message","message":{"role":"user","content":""}}`,
		}},
	} {
		t.Run(test.agent, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writeFixture(t, path, strings.Join(test.records, "\n"))
			detail, err := ParseNativeSessionFile(test.agent, path)
			if err != nil {
				t.Fatal(err)
			}
			want := SummaryOf(detail)
			item, ok, err := readJSONLMetadata(test.agent, path)
			if err != nil || !ok {
				t.Fatalf("metadata: %+v, %v", item, err)
			}
			for _, counts := range []bool{false, true} {
				got, err := summarizeJSONL(item, counts)
				if err != nil || got.FirstUserMessage != want.FirstUserMessage || got.LatestUserMessage != want.LatestUserMessage {
					t.Fatalf("counts=%v: summary=%+v, want=%+v, error=%v", counts, got, want, err)
				}
				if counts && got.MessageCount != want.MessageCount {
					t.Fatalf("count=%d, want=%d", got.MessageCount, want.MessageCount)
				}
			}
		})
	}
}
