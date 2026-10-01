package schema

// request_user_input: codex's tool for asking the user questions, sent as
// the server request MethodToolRequestUserInput. Experimental. On codex
// 0.159.3 the model is offered the tool in the Plan collaboration mode
// (turn/start collaborationMode {mode:"plan"}, which needs experimentalApi),
// or in default mode with the underDevelopment feature
// default_mode_request_user_input.

// ToolRequestUserInputParams is the request_user_input request payload.
type ToolRequestUserInputParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	// ItemID is the model's tool call id ("call_…").
	ItemID    string                         `json:"itemId"`
	Questions []ToolRequestUserInputQuestion `json:"questions"`
	// IsBlocking reports whether the turn waits for the answer.
	IsBlocking bool `json:"isBlocking"`
	// AutoResolutionMs is deprecated in favour of IsBlocking.
	AutoResolutionMs *int64 `json:"autoResolutionMs,omitempty"`
}

// ToolRequestUserInputQuestion is one question. Answers are keyed by ID.
type ToolRequestUserInputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	// IsOther means a free-text answer outside Options is accepted.
	IsOther bool `json:"isOther"`
	// IsSecret means the answer is sensitive; do not echo or log it.
	IsSecret bool `json:"isSecret"`
	// Options is nil for a free-text question.
	Options []ToolRequestUserInputOption `json:"options"`
}

// ToolRequestUserInputOption is a selectable answer. Answer with its Label.
type ToolRequestUserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ToolRequestUserInputResponse is the reply: answers keyed by question id.
// Codex ignores keys that are not question ids, so a reply keyed any other
// way reaches the model as no answer at all.
type ToolRequestUserInputResponse struct {
	Answers map[string]ToolRequestUserInputAnswer `json:"answers"`
}

// ToolRequestUserInputAnswer holds one question's answers: one entry for a
// single choice or free text, several for a multi-select.
type ToolRequestUserInputAnswer struct {
	Answers []string `json:"answers"`
}

// UserInputAnswers builds the reply from answers keyed by question id.
func UserInputAnswers(byQuestion map[string][]string) ToolRequestUserInputResponse {
	out := ToolRequestUserInputResponse{Answers: map[string]ToolRequestUserInputAnswer{}}
	for id, answers := range byQuestion {
		if answers == nil {
			answers = []string{}
		}
		out.Answers[id] = ToolRequestUserInputAnswer{Answers: answers}
	}
	return out
}

// UserInputDismissed is the reply for a user who closed the question
// without answering: no answers. The model is told it got none and the
// turn continues. Codex 0.159.3 treats an empty answer list and a JSON-RPC
// error response the same way; this is the schema-valid form.
func UserInputDismissed() ToolRequestUserInputResponse {
	return ToolRequestUserInputResponse{Answers: map[string]ToolRequestUserInputAnswer{}}
}
