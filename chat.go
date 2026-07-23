package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Codeium ChatMessage role enum values (field 2), from captured traffic.
const (
	roleUser      = 1
	roleAssistant = 2
	roleTool      = 4
)

// ---- OpenAI request shapes (subset we consume) ----

type oaiRequest struct {
	Model                  string          `json:"model"`
	Messages               []oaiMessage    `json:"messages"`
	Tools                  []oaiTool       `json:"tools"`
	ToolChoice             json.RawMessage `json:"tool_choice"`
	ParallelToolCalls      *bool           `json:"parallel_tool_calls"`
	Stream                 bool            `json:"stream"`
	ReasoningEffort        string          `json:"reasoning_effort"`
	ResolvedToolChoice     string          `json:"-"`
	LimitParallelToolCalls bool            `json:"-"`
	ToolCompatibilityError string          `json:"-"`
}

type oaiMessage struct {
	Role            string              `json:"role"`
	Content         json.RawMessage     `json:"content"`
	ToolCalls       []oaiToolCall       `json:"tool_calls"`
	ToolCallID      string              `json:"tool_call_id"`
	PreparedText    string              `json:"-"`
	ContentPrepared bool                `json:"-"`
	Images          []codeiumImageInput `json:"-"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// contentString flattens OpenAI content (string or array of parts) to text.
func (m oaiMessage) contentString() string {
	if m.ContentPrepared {
		return m.PreparedText
	}
	return compatibleMessageContent(m.Content)
}

// buildChatRequest converts an OpenAI chat completion request into a
// GetChatMessageRequest protobuf. Returns the encoded message and the resolved
// upstream model id.
func buildChatRequest(cfg providerConfig, jwt string, oai oaiRequest) ([]byte, string) {
	model := oai.Model
	if model == "" {
		model = "swe-1-7"
	}

	// Collect a system prompt from any leading system messages.
	system := cfg.systemPrompt
	var sysParts []string
	for _, m := range oai.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if c := m.contentString(); c != "" {
				sysParts = append(sysParts, c)
			}
		}
	}
	if len(sysParts) > 0 {
		system = strings.Join(sysParts, "\n\n")
	}
	if oai.LimitParallelToolCalls && len(oai.Tools) > 0 {
		system = strings.TrimSpace(system + "\n\nCall at most one tool in this assistant turn.")
	}

	var req pw
	req.msg(1, metadataForChat(cfg, jwt))
	req.str(2, system)

	for _, m := range oai.Messages {
		switch m.Role {
		case "system", "developer":
			continue
		case "user":
			req.msg(3, chatMessage(roleUser, "", m.contentString(), "", nil, m.Images))
		case "assistant":
			req.msg(3, chatMessage(roleAssistant, "bot-"+uuid.NewString(), m.contentString(), "", m.ToolCalls, m.Images))
		case "tool":
			req.msg(3, chatMessage(roleTool, "", m.contentString(), m.ToolCallID, nil, m.Images))
		default:
			req.msg(3, chatMessage(roleUser, "", m.contentString(), "", nil, m.Images))
		}
	}

	// Tool definitions (f10).
	for _, t := range oai.Tools {
		if t.Function.Name == "" {
			continue
		}
		var td pw
		td.str(1, t.Function.Name)
		td.str(2, t.Function.Description)
		if len(t.Function.Parameters) > 0 {
			td.str(3, string(t.Function.Parameters))
		}
		req.msg(10, td.bytes())
	}

	// Tool choice is normalized before encoding. A selected function is reduced
	// to a one-tool required request, which avoids relying on a provider-specific
	// forced-function wire representation.
	if len(oai.Tools) > 0 && oai.ResolvedToolChoice != "" {
		var tc pw
		tc.str(1, oai.ResolvedToolChoice)
		req.msg(12, tc.bytes())
	}

	// f7 / f8 / f9 / f13 — static client config incl. the Cascade capability
	// declaration the backend gates on. Appended verbatim (see staticconfig.go).
	req.raw(staticClientConfig)

	// f15 — trajectory/step counters { id, msgCount, 4, 14 }.
	var traj pw
	traj.str(1, uuid.NewString())
	traj.varintAlways(2, uint64(len(oai.Messages)))
	traj.varintAlways(3, 4)
	traj.varintAlways(4, 14)
	req.msg(15, traj.bytes())

	req.str(16, uuid.NewString()) // turn id
	req.varintAlways(20, 1)
	req.str(21, resolveModelWire(cfg.sessionToken, model, oai.ReasoningEffort)) // account model (+ effort) -> wire id
	req.str(22, uuid.NewString())                                               // conversation id
	return req.bytes(), model
}

// chatMessage encodes a single ChatMessage sub-message.
func chatMessage(
	role int,
	botID,
	content,
	toolCallID string,
	toolCalls []oaiToolCall,
	images []codeiumImageInput,
) []byte {
	var m pw
	m.str(1, botID)
	m.varintAlways(2, uint64(role))
	m.str(3, content)
	m.str(7, toolCallID)
	for i, tc := range toolCalls {
		m.bytesField(6, encodeToolCall(i, tc))
	}
	for _, image := range images {
		m.msg(10, encodeImageData(image))
	}
	return m.bytes()
}

// encodeImageData writes exa.codeium_common_pb.ImageData:
// f1 base64_data, f2 mime_type, f3 caption.
func encodeImageData(image codeiumImageInput) []byte {
	var imageMessage pw
	imageMessage.str(1, image.Base64Data)
	imageMessage.str(2, image.MIMEType)
	imageMessage.str(3, image.Caption)
	return imageMessage.bytes()
}

// encodeToolCall builds the nested tool-call message { f1 id, f2 name, f3 args }.
// The tool-call id is reused verbatim (Codeium correlates tool results to calls
// by exact string match on this id), so whatever id the client echoes back in the
// tool result also lands in f7 and matches. Only synthesise Codeium's native
// "functions.<name>:<index>" form when the client supplied no id.
func encodeToolCall(index int, tc oaiToolCall) []byte {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("functions.%s:%d", tc.Function.Name, index)
	}
	var w pw
	w.str(1, id)
	w.str(2, tc.Function.Name)
	if tc.Function.Arguments != "" {
		w.str(3, tc.Function.Arguments)
	}
	return w.bytes()
}

// respDelta holds one increment parsed from a streamed response frame.
//
// The backend streams several channels:
//   - f3 = the assistant's answer content (maps to OpenAI delta.content)
//   - f9 = the model's chain-of-thought (maps to delta.reasoning_content)
//   - f6 = a tool call { f1 id, f2 name, f3 args-delta }. The start frame carries
//     id+name; subsequent frames stream the JSON arguments in f3.
//
// The model id arrives nested in f7.f9.
type respDelta struct {
	content   string
	reasoning string
	model     string
	tools     []toolDelta
}

type toolDelta struct {
	id   string // f6.f1 (usually present only on a tool-call start frame)
	name string // f6.f2
	args string // f6.f3 (streamed argument delta)
}

// parseResponseFrame extracts content (f3), reasoning (f9), tool calls (f6), and
// model (f7.f9) from a GetChatMessageResponse frame body.
func parseResponseFrame(body []byte) respDelta {
	var d respDelta
	r := newPR(body)
	for !r.eof() {
		f, wire, sub, _, err := r.next()
		if err != nil {
			break
		}
		switch {
		case f == 3 && wire == 2:
			d.content += string(sub)
		case f == 9 && wire == 2:
			d.reasoning += string(sub)
		case f == 6 && wire == 2:
			var tool toolDelta
			tr := newPR(sub)
			for !tr.eof() {
				tf, tw, tsub, _, terr := tr.next()
				if terr != nil {
					break
				}
				if tw != 2 {
					continue
				}
				switch tf {
				case 1:
					tool.id = string(tsub)
				case 2:
					tool.name = string(tsub)
				case 3:
					tool.args += string(tsub)
				}
			}
			if tool.id != "" || tool.name != "" || tool.args != "" {
				d.tools = append(d.tools, tool)
			}
		case f == 7 && wire == 2:
			// nested metadata { f9: model }
			if m, err := parseFirstStringField(sub, 9); err == nil {
				d.model = m
			}
		}
	}
	return d
}
