package ollama

import "encoding/json"

type Event struct {
	Text            string
	Done            bool
	PromptEvalCount int
	EvalCount       int
}

// EventFromJSON extracts only documented text and usage fields. Unknown
// protocol fields remain opaque and are forwarded unchanged by the caller.
func EventFromJSON(data []byte) Event {
	var payload struct {
		Type            string          `json:"type"`
		Done            bool            `json:"done"`
		Response        json.RawMessage `json:"response"`
		PromptEvalCount int             `json:"prompt_eval_count"`
		EvalCount       int             `json:"eval_count"`
		Text            string          `json:"text"`
		Delta           json.RawMessage `json:"delta"`
		Message         struct {
			Content optionalText `json:"content"`
			Usage   usage        `json:"usage"`
		} `json:"message"`
		Choices []struct {
			Text    string  `json:"text"`
			Delta   content `json:"delta"`
			Message content `json:"message"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return Event{}
	}
	var responseText string
	var responseObject struct {
		Usage usage `json:"usage"`
	}
	if len(payload.Response) > 0 {
		_ = json.Unmarshal(payload.Response, &responseText)
		_ = json.Unmarshal(payload.Response, &responseObject)
	}
	var deltaText string
	var deltaObject struct {
		Content optionalText `json:"content"`
		Text    optionalText `json:"text"`
	}
	if len(payload.Delta) > 0 {
		_ = json.Unmarshal(payload.Delta, &deltaText)
		_ = json.Unmarshal(payload.Delta, &deltaObject)
	}
	event := Event{
		Text:            responseText + payload.Text + deltaText + string(deltaObject.Content) + string(deltaObject.Text) + string(payload.Message.Content),
		Done:            payload.Done || payload.Type == "response.completed" || payload.Type == "response.incomplete" || payload.Type == "response.failed" || payload.Type == "message_stop",
		PromptEvalCount: payload.PromptEvalCount,
		EvalCount:       payload.EvalCount,
	}
	for _, choice := range payload.Choices {
		event.Text += choice.Text + string(choice.Delta.Content) + string(choice.Message.Content)
	}
	mergeUsageFields(&event, payload.Usage)
	mergeUsageFields(&event, payload.Message.Usage)
	mergeUsageFields(&event, responseObject.Usage)
	return event
}

type content struct {
	Content optionalText `json:"content"`
}

type optionalText string

func (text *optionalText) UnmarshalJSON(data []byte) error {
	var value string
	if json.Unmarshal(data, &value) == nil {
		*text = optionalText(value)
	}
	return nil
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func mergeUsageFields(event *Event, value usage) {
	if n := max(value.PromptTokens, value.InputTokens); n > event.PromptEvalCount {
		event.PromptEvalCount = n
	}
	if n := max(value.CompletionTokens, value.OutputTokens); n > event.EvalCount {
		event.EvalCount = n
	}
}

func mergeUsage(total *Event, event Event) {
	if event.PromptEvalCount > total.PromptEvalCount {
		total.PromptEvalCount = event.PromptEvalCount
	}
	if event.EvalCount > total.EvalCount {
		total.EvalCount = event.EvalCount
	}
}
