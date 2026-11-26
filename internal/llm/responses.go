package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// ---------- a ChatGPT plan: OpenAI's Responses API with a sign-in ----------
//
// Sign in with ChatGPT lets an open-source app use a person's ChatGPT plan
// through POST /v1/responses with the sign-in's access token. The preview
// sets the request's shape: store false and stream true; the system prompt
// as instructions, not a message; no temperature or output cap; function
// tools only inside a namespace. See
// https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations.

type responses struct {
	cfg  Config
	base string
	hc   *http.Client
	// noEncrypted: the model refused to give its reasoning encrypted, so
	// it isn't asked to again.
	noEncrypted atomic.Bool
}

// toolNamespace is the namespace the repository tools are offered in.
const toolNamespace = "repo"

// responsesTurn is an assistant turn with tool calls as the server sent it:
// its output items (reasoning, text and calls), in order, sent back as
// they came to the model and sign-in that wrote them (see Message.Native).
// Their IDs stay: the API's input items take them, and Codex, the client
// this sign-in is made for, sends them back with store false too. The
// sign-in counts as well as the model: two models of one name can be
// signed in to two accounts, and one account's encrypted reasoning isn't
// for the other's.
type responsesTurn struct {
	model  string
	tokens TokenSource
	items  []json.RawMessage
}

// encryptedWords are how a server names the encrypted reasoning when it
// refuses to give it.
var encryptedWords = regexp.MustCompile(`(?i)encrypted|\binclude\b`)

func encryptedRefused(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusBadRequest && encryptedWords.MatchString(he.body)
}

func (c *responses) Chat(ctx context.Context, req Request) (*Response, error) {
	if c.cfg.Model == "" {
		return nil, errors.New("no model selected")
	}
	instructions, input := responsesInput(req.Messages, c.cfg.Model, c.cfg.Tokens)
	body := map[string]any{
		"model":  c.cfg.Model,
		"input":  input,
		"store":  false,
		"stream": true,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if !c.noEncrypted.Load() {
		// With nothing stored at OpenAI, a reasoning model's reasoning only
		// carries on to the next turn of a tool conversation if it's sent
		// back, which takes it encrypted.
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if len(req.Tools) > 0 {
		body["tools"] = []any{responsesNamespace(req.Tools)}
	}
	if req.Schema != nil && c.cfg.Structured {
		name := req.SchemaName
		if name == "" {
			name = "response"
		}
		body["text"] = map[string]any{"format": map[string]any{
			"type": "json_schema", "name": name, "schema": req.Schema, "strict": true,
		}}
	}
	start := time.Now()
	res, err := c.stream(ctx, body)
	if _, asked := body["include"]; asked && encryptedRefused(err) {
		c.noEncrypted.Store(true)
		delete(body, "include")
		res, err = c.stream(ctx, body)
	}
	if err != nil {
		return nil, toolsRefused(req, err)
	}
	var text, refusal, reasoning strings.Builder
	var calls []ToolCall
	turn := responsesTurn{model: c.cfg.Model, tokens: c.cfg.Tokens}
	truncated := false
	for _, raw := range res.Output {
		var it struct {
			Type      string          `json:"type"`
			Status    string          `json:"status"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		switch it.Type {
		case "message":
			for _, p := range it.Content {
				switch p.Type {
				case "output_text":
					text.WriteString(p.Text)
				case "refusal":
					refusal.WriteString(p.Refusal)
				}
			}
			turn.items = append(turn.items, raw)
		case "function_call":
			if it.Status != "" && it.Status != "completed" {
				// Cut off while its arguments were being written: not run,
				// and not sent back.
				truncated = true
				continue
			}
			id := it.CallID
			if id == "" {
				id = fmt.Sprintf("call_%d", len(calls))
			}
			calls = append(calls, ToolCall{ID: id, Name: bareToolName(it.Name), Arguments: rawArgs(it.Arguments)})
			turn.items = append(turn.items, raw)
		case "reasoning":
			for _, s := range it.Summary {
				reasoning.WriteString(s.Text)
			}
			if it.Encrypted != "" { // without it, there's nothing to send back
				turn.items = append(turn.items, raw)
			}
		}
	}
	if res.Status == "incomplete" && res.IncompleteDetails != nil {
		switch res.IncompleteDetails.Reason {
		case "max_output_tokens":
			truncated = true
		case "content_filter":
			return nil, fmt.Errorf("%w (content filter)", ErrRefused)
		}
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" && refusal.Len() > 0 {
		return nil, fmt.Errorf("%w (%s)", ErrRefused, textutil.Truncate(refusal.String(), 300))
	}
	out := &Response{
		Content:          text.String(),
		ToolCalls:        calls,
		Reasoning:        reasoning.String(),
		PromptTokens:     res.Usage.InputTokens,
		CompletionTokens: res.Usage.OutputTokens,
		Truncated:        truncated,
		Duration:         time.Since(start),
	}
	if len(calls) > 0 {
		out.Native = turn
	}
	return out, nil
}

// responsesInput splits messages into the instructions (the system prompt:
// system messages are refused in the input) and the input items for model,
// signed in with tokens.
func responsesInput(msgs []Message, model string, tokens TokenSource) (string, []any) {
	var system []string
	in := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "tool":
			in = append(in, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
		case "assistant":
			// The model's own turn goes back as it came, its reasoning
			// with it; another model, or another sign-in, gets just the
			// text and the calls.
			if t, ok := m.Native.(responsesTurn); ok && t.model == model && t.tokens == tokens {
				for _, it := range t.items {
					in = append(in, it)
				}
				continue
			}
			if strings.TrimSpace(m.Content) != "" {
				in = append(in, map[string]any{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				in = append(in, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": string(argsObject(tc.Arguments))})
			}
		default:
			in = append(in, map[string]any{"role": "user", "content": m.Content})
		}
	}
	return strings.Join(system, "\n\n"), in
}

func responsesNamespace(tools []Tool) map[string]any {
	fns := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fns = append(fns, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters})
	}
	return map[string]any{"type": "namespace", "name": toolNamespace,
		"description": "Read-only tools for the pull request's repository.", "tools": fns}
}

// bareToolName is a call's tool name without the namespace, should the
// server put it in front.
func bareToolName(name string) string {
	for _, sep := range []string{".", "__", "/", ":"} {
		if n, ok := strings.CutPrefix(name, toolNamespace+sep); ok {
			return n
		}
	}
	return name
}

// responseObject is the part of a response the bot reads.
type responseObject struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// send makes one request with the sign-in's token, asking for another
// token and trying once more if the server refuses the one it had.
func (c *responses) send(ctx context.Context, method, u string, body []byte, accept string) (*http.Response, error) {
	refused := ""
	for {
		tok, err := c.cfg.Tokens.Token(ctx, refused)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", accept)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || refused != "" {
			return resp, nil
		}
		resp.Body.Close()
		refused = tok
	}
}

// failed reads a refused request's answer as an error.
func failed(resp *http.Response, method, u string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return planError(&httpError{method: method, url: u, status: resp.StatusCode, body: textutil.Truncate(strings.TrimSpace(string(data)), 500)})
}

// stream posts body and reads the event stream up to its terminal event.
func (c *responses) stream(ctx context.Context, body map[string]any) (*responseObject, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := c.base + "/responses"
	resp, err := c.send(ctx, http.MethodPost, u, b, "text/event-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, failed(resp, http.MethodPost, u)
	}
	return readEvents(io.LimitReader(resp.Body, 64<<20), u)
}

// readEvents reads server-sent events until the response is completed (or
// incomplete), and returns it. A failure in the stream is an error.
func readEvents(r io.Reader, u string) (*responseObject, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var data strings.Builder
	for {
		line, rerr := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			if res, done, err := streamEvent(data.String(), u); done || err != nil {
				return res, err
			}
			data.Reset()
		}
		if rerr == io.EOF {
			if data.Len() > 0 {
				if res, done, err := streamEvent(data.String(), u); done || err != nil {
					return res, err
				}
			}
			return nil, errors.New("the answer's stream ended before it was complete")
		}
		if rerr != nil {
			return nil, fmt.Errorf("read the answer: %w", rerr)
		}
	}
}

// streamEvent handles one event: done once the response is complete.
func streamEvent(data, u string) (*responseObject, bool, error) {
	if data == "[DONE]" {
		return nil, false, nil
	}
	var ev struct {
		Type     string          `json:"type"`
		Response *responseObject `json:"response"`
		Code     string          `json:"code"`
		Message  string          `json:"message"`
		Param    string          `json:"param"`
		Error    *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, false, nil // not an event the bot reads
	}
	switch ev.Type {
	case "response.completed", "response.incomplete":
		if ev.Response == nil {
			return nil, true, errors.New("the server's last event had no response")
		}
		return ev.Response, true, nil
	case "response.failed":
		code, msg := "", "the response failed"
		if ev.Response != nil && ev.Response.Error != nil {
			code, msg = ev.Response.Error.Code, ev.Response.Error.Message
		}
		return nil, true, streamError(code, msg, "", u)
	case "error":
		code, msg, param := ev.Code, ev.Message, ev.Param
		if ev.Error != nil {
			code, msg, param = ev.Error.Code, ev.Error.Message, ev.Error.Param
		}
		return nil, true, streamError(code, msg, param, u)
	}
	return nil, false, nil
}

// streamStatus gives a failure reported in the stream the status the same
// failure has before it, so it's handled alike (a refused tool, say).
var streamStatus = map[string]int{
	"subscription_sharing_unsupported_capability": http.StatusBadRequest,
	"subscription_sharing_route_not_supported":    http.StatusForbidden,
	"subscription_sharing_user_not_eligible":      http.StatusForbidden,
	"subscription_sharing_usage_limit_exceeded":   http.StatusTooManyRequests,
	"subscription_sharing_usage_unavailable":      http.StatusServiceUnavailable,
	"rate_limit_exceeded":                         http.StatusTooManyRequests,
	"invalid_request_error":                       http.StatusBadRequest,
}

func streamError(code, msg, param, u string) error {
	status, ok := streamStatus[code]
	if !ok {
		status = http.StatusBadGateway
	}
	body := strings.TrimSpace(strings.Join([]string{code, msg}, ": "))
	if param != "" {
		body += " (param " + param + ")"
	}
	return planError(&httpError{method: http.MethodPost, url: u, status: status, body: textutil.Truncate(body, 500)})
}

// planHints say what the plan-usage failures mean for the person running
// the bot (see .../errors-and-recovery).
var planHints = map[string]string{
	"subscription_sharing_usage_limit_exceeded":   "the ChatGPT plan's usage limit for this app is used up; see https://chatgpt.com/settings/usage",
	"subscription_sharing_user_not_eligible":      "this ChatGPT account or workspace can't use its plan in other apps",
	"subscription_sharing_usage_unavailable":      "ChatGPT couldn't check the plan's usage just now; try again later",
	"subscription_sharing_unsupported_capability": "the ChatGPT plan can't take part of this request",
	"subscription_sharing_route_not_supported":    "the ChatGPT plan can't be used at this address",
}

// planError puts what a known plan-usage failure means in front of it.
func planError(he *httpError) error {
	for code, hint := range planHints {
		if strings.Contains(he.body, code) {
			he.body = hint + ": " + he.body
			break
		}
	}
	if he.status == http.StatusUnauthorized {
		he.body = "the ChatGPT sign-in was refused; sign in again on the Models page: " + he.body
	}
	return he
}

// getJSON is a GET with the sign-in's token.
func (c *responses) getJSON(ctx context.Context, u string, out any) error {
	resp, err := c.send(ctx, http.MethodGet, u, nil, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return failed(resp, http.MethodGet, u)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", u, err)
	}
	return nil
}

// catalog lists the models the signed-in account may use: plan usage
// answers with a models array of slugs, the API with a data array of IDs.
// Models not meant to be shown (visibility other than "list") are left out.
func (c *responses) catalog(ctx context.Context) (map[string]map[string]any, []string, error) {
	var out struct {
		Models []map[string]any `json:"models"`
		Data   []map[string]any `json:"data"`
	}
	if err := c.getJSON(ctx, c.base+"/models", &out); err != nil {
		return nil, nil, err
	}
	byID := map[string]map[string]any{}
	var ids []string
	for _, m := range append(out.Models, out.Data...) {
		if v, ok := m["visibility"].(string); ok && v != "list" {
			continue
		}
		id, _ := m["slug"].(string)
		if id == "" {
			id, _ = m["id"].(string)
		}
		if id == "" || byID[id] != nil {
			continue
		}
		byID[id] = m
		ids = append(ids, id)
	}
	return byID, ids, nil
}

func (c *responses) Models(ctx context.Context) ([]string, error) {
	_, ids, err := c.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// Limits reads the model's limits from the account's model catalog, if it
// gives them.
func (c *responses) Limits(ctx context.Context) (Limits, error) {
	if c.cfg.Model == "" {
		return Limits{}, errors.New("no model selected")
	}
	byID, _, err := c.catalog(ctx)
	if err != nil {
		return Limits{}, err
	}
	m, ok := byID[c.cfg.Model]
	if !ok {
		return Limits{}, fmt.Errorf("the ChatGPT account's models don't include %q; check the name, or press Load models", c.cfg.Model)
	}
	if l := limitsFrom(m); l.Context > 0 || l.Output > 0 {
		return l, nil
	}
	return Limits{}, errNoLimits
}
