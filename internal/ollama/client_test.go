package ollama

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPrepareNativePinsModelAndCeiling(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareNative([]byte(`{"model":"client-model","prompt":"hi","options":{"num_predict":999}}`), true, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"model":"configured"`) || !strings.Contains(got, `"num_predict":100`) || !strings.Contains(got, `"stream":true`) {
		t.Fatalf("prepared body: %s", got)
	}
}

func TestPrepareCompatibleKeepsShapeAndCapsOutput(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareCompatible([]byte(`{"model":"client","messages":[],"stream":true,"max_tokens":999}`), "/v1/chat/completions", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"model":"configured"`) || !strings.Contains(got, `"max_tokens":100`) || !strings.Contains(got, `"stream":true`) {
		t.Fatalf("prepared body: %s", got)
	}
	if strings.Contains(got, "num_predict") {
		t.Fatalf("native options leaked into compatible body: %s", got)
	}
}

func TestOutputLimitUsesEffectiveField(t *testing.T) {
	body := []byte(`{"max_tokens":10,"max_completion_tokens":80,"max_output_tokens":40}`)
	if got := OutputLimit(body, 100); got != 10 {
		t.Fatalf("output limit = %d, want 10", got)
	}
}

func TestPrepareCompatibleAddsMissingEffectiveLimit(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareCompatible([]byte(`{"messages":[]}`), "/v1/chat/completions", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"max_tokens":100`) || OutputLimit(body, 0) != 100 {
		t.Fatalf("prepared body did not apply the safety limit: %s", body)
	}
}

func TestPrepareCompatibleIgnoresUnrelatedMaxFields(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareCompatible([]byte(`{"messages":[],"max_output_tokens":7}`), "/v1/chat/completions", 100)
	if err != nil {
		t.Fatal(err)
	}
	if OutputLimit(body, 0) != 100 {
		t.Fatalf("unrelated max field defined the limit: %s", body)
	}
}

func TestPrepareCompatibleResponsesNormalizesLimitField(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareCompatible([]byte(`{"input":"x","max_tokens":3}`), "/v1/responses", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"max_tokens"`) || !strings.Contains(string(body), `"max_output_tokens":100`) {
		t.Fatalf("responses body kept the wrong limit field: %s", body)
	}
	if OutputLimit(body, 0) != 100 {
		t.Fatalf("reservation limit does not match enforced cap: %s", body)
	}
}

func TestPrepareCompatibleDoesNotRaiseEffectiveLimit(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareCompatible([]byte(`{"messages":[],"max_tokens":5,"max_output_tokens":100}`), "/v1/chat/completions", 128)
	if err != nil {
		t.Fatal(err)
	}
	if OutputLimit(body, 128) != 5 {
		t.Fatalf("prepared body raised requested limit: %s", body)
	}
}

func TestPrepareModelDoesNotAddGenerationLimit(t *testing.T) {
	c := Client{Model: "configured"}
	body, err := c.PrepareModel([]byte(`{"model":"client","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"model":"configured"`) || strings.Contains(got, "max_tokens") {
		t.Fatalf("prepared embedding body: %s", got)
	}
}

func TestStreamNDJSONPreservesFramingAndUsage(t *testing.T) {
	input := "{\"response\":\"hi\",\"done\":false}\r\n{\"done\":true,\"prompt_eval_count\":3,\"eval_count\":4}\r\n"
	c := Client{}
	var output strings.Builder
	usage, err := c.StreamNDJSON(strings.NewReader(input), func(raw []byte, _ Event) error {
		output.Write(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != input {
		t.Fatalf("framing changed: %q", output.String())
	}
	if usage.PromptEvalCount != 3 || usage.EvalCount != 4 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestStreamSSEPreservesFramesAndMergesUsage(t *testing.T) {
	input := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\r\n\r\n" +
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\r\n\r\n" +
		"event: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\r\n\r\n" +
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	c := Client{}
	var output strings.Builder
	var text string
	usage, err := c.StreamSSE(strings.NewReader(input), func(raw []byte, event Event) error {
		output.Write(raw)
		text += event.Text
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != input {
		t.Fatalf("framing changed: %q", output.String())
	}
	if text != "hi" || usage.PromptEvalCount != 3 || usage.EvalCount != 4 {
		t.Fatalf("text %q, usage: %+v", text, usage)
	}
}

func TestStreamKeepsTerminalUsageWhenCallbackFails(t *testing.T) {
	c := Client{}
	wantErr := io.ErrClosedPipe
	usage, err := c.StreamNDJSON(strings.NewReader("{\"done\":true,\"eval_count\":9}\n"), func([]byte, Event) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("stream error = %v, want %v", err, wantErr)
	}
	if usage.EvalCount != 9 {
		t.Fatalf("terminal usage lost after callback failure: %+v", usage)
	}
}

func TestEventFromJSONReadsResponsesStringDelta(t *testing.T) {
	event := EventFromJSON([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	if event.Text != "hello" {
		t.Fatalf("response delta text: %q", event.Text)
	}
}
