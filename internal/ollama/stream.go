package ollama

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// StreamNDJSON preserves every byte while parsing a trimmed copy of each line.
// It returns the merged usage observed across the stream.
func (c *Client) StreamNDJSON(reader io.Reader, onEvent func([]byte, Event) error) (Event, error) {
	buffered := bufio.NewReader(reader)
	limit := c.bodyLineLimit()
	var usage Event
	done := false
	for {
		raw, err := readLine(buffered, limit)
		if len(raw) > 0 {
			event := EventFromJSON(bytes.TrimSpace(raw))
			mergeUsage(&usage, event)
			done = done || event.Done
			if callbackErr := onEvent(raw, event); callbackErr != nil {
				return usage, callbackErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return usage, fmt.Errorf("read Ollama stream: %w", err)
		}
	}
	if !done {
		return usage, fmt.Errorf("Ollama stream ended without a final usage event")
	}
	return usage, nil
}

func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return nil, fmt.Errorf("stream line exceeds %d bytes", limit)
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// StreamSSE preserves complete SSE frames while observing their data payloads.
// It returns the merged usage observed across the stream.
func (c *Client) StreamSSE(reader io.Reader, onEvent func([]byte, Event) error) (Event, error) {
	buffered := bufio.NewReader(reader)
	limit := c.bodyLineLimit()
	var usage Event
	done := false
	for {
		frame, data, err := readSSEFrame(buffered, limit)
		if err == io.EOF {
			break
		}
		if err != nil {
			return usage, fmt.Errorf("read Ollama SSE stream: %w", err)
		}
		event := EventFromJSON(data)
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			event.Done = true
		}
		mergeUsage(&usage, event)
		done = done || event.Done
		if callbackErr := onEvent(frame, event); callbackErr != nil {
			return usage, callbackErr
		}
	}
	if !done {
		return usage, fmt.Errorf("Ollama SSE stream ended without a terminal event")
	}
	return usage, nil
}

func (c *Client) bodyLineLimit() int {
	if c.MaxBodyLine > 0 {
		return c.MaxBodyLine
	}
	return 2 << 20
}

func readSSEFrame(reader *bufio.Reader, limit int) ([]byte, []byte, error) {
	var frame bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if frame.Len()+len(line) > limit {
			return nil, nil, fmt.Errorf("SSE frame exceeds %d bytes", limit)
		}
		frame.Write(line)
		if err != nil {
			if err == io.EOF && frame.Len() == 0 {
				return nil, nil, io.EOF
			}
			if err != io.EOF {
				return nil, nil, err
			}
			break
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	var data []byte
	for _, line := range bytes.Split(frame.Bytes(), []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))...)
	}
	return frame.Bytes(), data, nil
}
