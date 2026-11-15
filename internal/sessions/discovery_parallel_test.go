package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverJSONLParallelMatchesSerialResultsAndErrors(t *testing.T) {
	root := t.TempDir()
	valid := func(id, padding string) string {
		return fmt.Sprintf(`{"sessionId":%q,"cwd":%q,"timestamp":"2026-11-14T01:02:03Z","type":"user","message":{"content":%q},"padding":%q}`,
			id, "/work/"+id, "Request "+id, padding)
	}
	files := []struct {
		dir, name, content string
	}{
		{"a", "00-small.jsonl", valid("a-small", "")},
		{"a", "01-large.jsonl", valid("a-large", strings.Repeat("x", 256*1024))},
		{"a", "02-mixed.jsonl", "not JSON\n" + valid("a-mixed", strings.Repeat("m", 2048))},
		{"a", "03-unidentified.jsonl", `{"type":"assistant","message":{"content":"no session metadata"}}`},
		{"a", "04-malformed.jsonl", "{broken JSON\n"},
		{"b", "00-large.jsonl", valid("b-large", strings.Repeat("b", 128*1024))},
		{"b", "01-small.jsonl", valid("b-small", "")},
		{"b", "02-unidentified.jsonl", `{"cwd":"/work/no-id","type":"assistant"}`},
		{"b", "03-mixed.jsonl", `{"sessionId":"b-mixed","cwd":"/work/b-mixed"}` + "\nnot JSON\n"},
		{"b", "04-malformed.jsonl", "not JSON\n{also broken\n"},
		{"c", "00-small.jsonl", valid("c-small", strings.Repeat("c", 64))},
		{"c", "01-large.jsonl", valid("c-large", strings.Repeat("z", 64*1024))},
		{"c", "02-mixed.jsonl", "{}\n" + valid("c-mixed", strings.Repeat("q", 4096))},
		{"c", "03-unidentified.jsonl", `{"type":"user","message":{"content":"no session id"}}`},
		{"c", "04-malformed.jsonl", "{\n"},
	}
	for _, file := range files {
		writeFixture(t, filepath.Join(root, file.dir, file.name), file.content)
	}
	directoryPaths := []string{
		filepath.Join(root, "a", "05-directory.jsonl"),
		filepath.Join(root, "b", "05-directory.jsonl"),
		filepath.Join(root, "c", "05-directory.jsonl"),
	}
	for _, path := range directoryPaths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	patterns := []string{
		filepath.Join(root, "a", "*.jsonl"),
		filepath.Join(root, "["),
		filepath.Join(root, "b", "*.jsonl"),
		filepath.Join(root, "c", "*.jsonl"),
	}
	var wantItems []Summary
	var wantFailures []error
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			wantFailures = append(wantFailures, err)
			continue
		}
		for _, path := range paths {
			item, ok, err := readJSONLMetadata("claude", path)
			if err != nil {
				wantFailures = append(wantFailures, err)
			} else if ok {
				wantItems = append(wantItems, item)
			}
		}
	}

	gotItems, gotErr := discoverJSONL("claude", patterns...)
	if !reflect.DeepEqual(gotItems, wantItems) {
		t.Fatalf("items differ from serial metadata reads:\n got: %#v\nwant: %#v", gotItems, wantItems)
	}
	wantErr := errors.Join(wantFailures...)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("error = %v, want %v", gotErr, wantErr)
	}
	if gotErr == nil {
		return
	}
	if gotErr.Error() != wantErr.Error() {
		t.Fatalf("error = %q, want ordered serial errors %q", gotErr, wantErr)
	}
	if !errors.Is(gotErr, filepath.ErrBadPattern) {
		t.Fatalf("joined error lost malformed-pattern cause: %v", gotErr)
	}
	joined, ok := gotErr.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != len(wantFailures) {
		t.Fatalf("joined causes = %v, want %d ordered causes", gotErr, len(wantFailures))
	}
	for i, cause := range joined.Unwrap() {
		if cause.Error() != wantFailures[i].Error() {
			t.Fatalf("cause %d = %q, want %q", i, cause, wantFailures[i])
		}
	}
	previous := -1
	for _, path := range directoryPaths {
		index := strings.Index(gotErr.Error(), path)
		if index <= previous {
			t.Fatalf("directory error paths are missing or out of order: %v", gotErr)
		}
		previous = index
	}
}
