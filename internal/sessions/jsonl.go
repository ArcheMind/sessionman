package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func readJSONL(path string) ([]map[string]any, error) {
	var records []map[string]any
	err := scanJSONL(path, func(record map[string]any) bool {
		records = append(records, record)
		return false
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// scanJSONL visits valid JSON objects until EOF or the visitor returns true.
// Native records can contain large tool outputs or images, so read one line at
// a time without Scanner's token-size limit. Invalid lines (including a partial
// final record in an active session) are skipped without hiding I/O errors.
func scanJSONL(path string, visit func(map[string]any) bool) error {
	return scanJSONLLines(path, func(line []byte) bool {
		var record map[string]any
		return json.Unmarshal(line, &record) == nil && visit(record)
	})
}

func scanJSONLLines(path string, visit func([]byte) bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return scanJSONLReader(file, visit)
}

func scanJSONLReader(source io.Reader, visit func([]byte) bool) error {
	reader := bufio.NewReaderSize(source, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if visit(line) {
			return nil
		}
		if err == io.EOF {
			return nil
		}
	}
}

func scanJSONLReverse(path string, visit func([]byte) bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("read JSONL %s: is a directory", path)
	}
	return scanJSONLReverseReader(file, info.Size(), visit)
}

func scanJSONLReverseReader(reader io.ReaderAt, size int64, visit func([]byte) bool) error {
	const blockSize = 4 * 1024
	readSize := int64(blockSize)
	var fragments [][]byte
	join := func(prefix []byte) []byte {
		if len(fragments) == 0 {
			return prefix
		}
		size := len(prefix)
		for _, fragment := range fragments {
			size += len(fragment)
		}
		line := make([]byte, 0, size)
		line = append(line, prefix...)
		for i := len(fragments) - 1; i >= 0; i-- {
			line = append(line, fragments[i]...)
		}
		return line
	}
	for offset := size; offset > 0; {
		count := min(readSize, offset)
		offset -= count
		block := make([]byte, count)
		n, err := reader.ReadAt(block, offset)
		if err != nil && err != io.EOF {
			return err
		}
		block = block[:n]
		end := len(block)
		for end > 0 {
			index := bytes.LastIndexByte(block[:end], '\n')
			if index < 0 {
				break
			}
			if visit(join(block[index+1 : end])) {
				return nil
			}
			fragments = nil
			end = index
		}
		fragments = append(fragments, block[:end])
		if end == len(block) {
			readSize = min(readSize*2, 64*1024)
		} else {
			readSize = blockSize
		}
	}
	if len(fragments) > 0 {
		visit(join(nil))
	}
	return nil
}
