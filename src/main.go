package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	chatURL   = "https://opencode.ai/zen/v1/chat/completions"
	modelsURL = "https://opencode.ai/zen/v1/models"
	listen    = "127.0.0.1:6446"
	apiKeyEnv = "OPENCODE_API_KEY"
)

var catalogPath = filepath.Join(os.Getenv("HOME"), ".local", "share", "opencode", "codex-models.json")
var opencodeAPIKey = strings.TrimSpace(os.Getenv(apiKeyEnv))

const modelsTTL = 60 * time.Second

var (
	modelsMu   sync.Mutex
	modelsInfo []map[string]any
	modelsTs   time.Time
)

func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func isFreeModel(id string) bool {
	return strings.HasSuffix(id, "-free") || id == "big-pickle"
}

func buildModelInfo(m map[string]any) map[string]any {
	slug, _ := m["id"].(string)
	name, _ := m["name"].(string)
	if name == "" {
		name = strings.ReplaceAll(slug, "-", " ")
	}
	return map[string]any{
		"slug":                    slug,
		"display_name":            name,
		"description":             "Free model via OpenCode Zen",
		"default_reasoning_level": "medium",
		"supported_reasoning_levels": []map[string]any{
			{"effort": "low", "description": "Fast responses with lighter reasoning"},
			{"effort": "medium", "description": "Balances speed and reasoning depth"},
			{"effort": "high", "description": "Greater reasoning depth for complex problems"},
		},
		"shell_type":                     "shell_command",
		"visibility":                     "list",
		"minimal_client_version":         []int{0, 120, 0},
		"supported_in_api":               true,
		"priority":                       100,
		"upgrade":                        nil,
		"base_instructions":              "",
		"supports_reasoning_summaries":   false,
		"support_verbosity":              false,
		"default_verbosity":              nil,
		"apply_patch_tool_type":          nil,
		"truncation_policy":              map[string]any{"mode": "bytes", "limit": 100000},
		"supports_parallel_tool_calls":   true,
		"supports_image_detail_original": false,
		"context_window":                 200000,
		"experimental_supported_tools":   []any{},
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fetchZenModels() ([]map[string]any, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	applyZenAuth(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models status %d", resp.StatusCode)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var free []map[string]any
	for _, m := range payload.Data {
		id, _ := m["id"].(string)
		if isFreeModel(id) {
			free = append(free, m)
		}
	}
	return free, nil
}

func writeCodexCatalog(free []map[string]any) error {
	models := make([]map[string]any, 0, len(free))
	for _, m := range free {
		models = append(models, buildModelInfo(m))
	}
	data, err := json.MarshalIndent(map[string]any{"models": models}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0o755); err != nil {
		return err
	}
	tmp := catalogPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, catalogPath)
}

func getFreeModels(force bool) []map[string]any {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if force || modelsInfo == nil || time.Since(modelsTs) > modelsTTL {
		if free, err := fetchZenModels(); err != nil {
			log.Printf("models fetch failed: %v", err)
		} else {
			newIDs := make([]string, 0, len(free))
			for _, m := range free {
				newIDs = append(newIDs, m["id"].(string))
			}
			changed := true
			if modelsInfo != nil {
				oldIDs := make([]string, 0, len(modelsInfo))
				for _, m := range modelsInfo {
					oldIDs = append(oldIDs, m["id"].(string))
				}
				changed = !equalStrings(newIDs, oldIDs)
			}
			modelsInfo = free
			modelsTs = time.Now()
			if changed {
				log.Printf("free models changed: %v", newIDs)
				if err := writeCodexCatalog(free); err != nil {
					log.Printf("catalog write failed: %v", err)
				}
			}
		}
	}
	return modelsInfo
}

func modelsWatcher() {
	for {
		time.Sleep(modelsTTL)
		getFreeModels(true)
	}
}

func applyZenAuth(req *http.Request) {
	if opencodeAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opencodeAPIKey)
	}
}

func parseContentParts(parts []any) []map[string]any {
	var out []map[string]any
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		t, _ := pm["type"].(string)
		switch t {
		case "input_text", "output_text", "text":
			text, _ := pm["text"].(string)
			out = append(out, map[string]any{"type": "text", "text": text})
		case "input_image":
			url, _ := pm["image_url"].(string)
			if url == "" {
				if f, ok := pm["file_url"].(string); ok {
					url = f
				}
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	return out
}

func parseInput(raw any, instructions string) []map[string]any {
	var messages []map[string]any
	if instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch in := raw.(type) {
	case string:
		if in != "" {
			messages = append(messages, map[string]any{"role": "user", "content": in})
		}
	case []any:
		for _, it := range in {
			item, ok := it.(map[string]any)
			if !ok {
				messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprint(it)})
				continue
			}
			itype, _ := item["type"].(string)
			switch itype {
			case "function_call":
				callID := item["call_id"]
				if callID == nil {
					callID = item["id"]
				}
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				messages = append(messages, map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{map[string]any{
						"id": callID, "type": "function",
						"function": map[string]any{"name": name, "arguments": args},
					}},
				})
			case "function_call_output":
				callID := item["call_id"]
				if callID == nil {
					callID = item["id"]
				}
				output, _ := item["output"].(string)
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
			default:
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				switch content := item["content"].(type) {
				case string:
					if content != "" {
						messages = append(messages, map[string]any{"role": role, "content": content})
					}
				case []any:
					if parts := parseContentParts(content); len(parts) > 0 {
						messages = append(messages, map[string]any{"role": role, "content": parts})
					}
				}
			}
		}
	}
	return messages
}

func toAnySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func parseTools(tools []any) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := tm["type"].(string); typ != "function" {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			fn = tm
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		strict, _ := fn["strict"].(bool)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": desc, "parameters": params, "strict": strict,
			},
		})
	}
	return out
}

func sse(w http.ResponseWriter, event string, data any) {
	payload, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	flush(w)
}

func flush(w http.ResponseWriter) {
	http.NewResponseController(w).Flush()
}

func internalTools() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "web_search",
				"description": "Search the web for current, up-to-date information. Returns clean text from the top search results. Use when you need facts, news, prices, or anything that may be beyond your training data.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":      map[string]any{"type": "string", "description": "A natural-language search query describing the information or page you want."},
						"numResults": map[string]any{"type": "number", "description": "Number of results to return (default 5)."},
					},
					"required": []any{"query"},
				},
				"strict": false,
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "webfetch",
				"description": "Fetch the full readable content of one or more URLs as clean text or markdown. Use when you have a specific URL and need to read its contents.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"urls":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of URLs to fetch."},
						"maxCharacters": map[string]any{"type": "number", "description": "Maximum characters to retrieve per URL (default 8000)."},
					},
					"required": []any{"urls"},
				},
				"strict": false,
			},
		},
	}
}

func mcpCall(baseURL, toolName string, args map[string]any) (string, error) {
	client := &http.Client{Timeout: 35 * time.Second}
	initBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "opencode-zen-bridge", "version": "1.0"},
		},
	})
	initReq, err := http.NewRequest(http.MethodPost, baseURL, bytes.NewReader(initBody))
	if err != nil {
		return "", err
	}
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("Accept", "application/json, text/event-stream")
	initResp, err := client.Do(initReq)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mcp init status %d", initResp.StatusCode)
	}
	sessionID := initResp.Header.Get("Mcp-Session-Id")

	callBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": toolName, "arguments": args},
	})
	callReq, err := http.NewRequest(http.MethodPost, baseURL, bytes.NewReader(callBody))
	if err != nil {
		return "", err
	}
	callReq.Header.Set("Content-Type", "application/json")
	callReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		callReq.Header.Set("Mcp-Session-Id", sessionID)
	}
	callResp, err := client.Do(callReq)
	if err != nil {
		return "", err
	}
	defer callResp.Body.Close()
	raw, err := io.ReadAll(callResp.Body)
	if err != nil {
		return "", err
	}
	if callResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mcp call status %d: %s", callResp.StatusCode, string(raw))
	}
	full := ""
	if strings.Contains(string(raw), "data:") {
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
		var lines []string
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		full = strings.Join(lines, "\n")
	} else {
		full = string(raw)
	}
	var msg struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(full), &msg); err != nil {
		return "", err
	}
	if msg.Error != nil {
		return "", fmt.Errorf("mcp error: %s", msg.Error.Message)
	}
	var texts []string
	for _, c := range msg.Result.Content {
		texts = append(texts, c.Text)
	}
	out := strings.Join(texts, "\n")
	if msg.Result.IsError || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("mcp tool returned empty or error")
	}
	return out, nil
}

func htmlToText(s string) string {
	var b strings.Builder
	i, n := 0, len(s)
	for i < n {
		if s[i] == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			tag := strings.ToLower(strings.TrimSpace(s[i+1 : i+end]))
			fields := strings.Fields(tag)
			if len(fields) > 0 {
				name := fields[0]
				if name == "script" || name == "style" || strings.HasPrefix(name, "/script") || strings.HasPrefix(name, "/style") {
					closing := "</" + strings.TrimPrefix(name, "/") + ">"
					if ci := strings.Index(s[i+end+1:], closing); ci >= 0 {
						i += end + 1 + ci + len(closing)
						continue
					}
				}
			}
			i += end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func simpleFetchText(url string, maxChars int) (string, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxChars)+4096))
	if err != nil {
		return "", err
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = htmlToText(text)
	}
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return "URL: " + url + "\n" + text, nil
}

func execWebSearch(query string, numResults int) string {
	if query == "" {
		return "Web search error: no query provided."
	}
	if numResults < 1 {
		numResults = 5
	}
	if out, err := mcpCall("https://mcp.exa.ai/mcp", "web_search_exa", map[string]any{"query": query, "numResults": numResults}); err == nil {
		return out
	}
	if out, err := mcpCall("https://mcp.firecrawl.dev/v2/mcp", "firecrawl_search", map[string]any{"query": query, "limit": numResults}); err == nil {
		return out
	}
	return "Web search failed for query: " + query
}

func execWebFetch(urls []string, maxChars int) string {
	if len(urls) == 0 {
		return "webfetch error: no URLs provided."
	}
	if maxChars < 1 {
		maxChars = 8000
	}
	if out, err := mcpCall("https://mcp.exa.ai/mcp", "web_fetch_exa", map[string]any{"urls": urls, "maxCharacters": maxChars}); err == nil {
		return out
	}
	var parts []string
	for _, u := range urls {
		if t, err := simpleFetchText(u, maxChars); err == nil {
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n---\n\n")
	}
	for _, u := range urls {
		if out, err := mcpCall("https://mcp.firecrawl.dev/v2/mcp", "firecrawl_scrape", map[string]any{"url": u, "formats": []any{"markdown"}}); err == nil {
			parts = append(parts, out)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n---\n\n")
	}
	return "Failed to fetch the requested URL(s)."
}

func execInternalTool(name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		args = map[string]any{}
	}
	switch name {
	case "web_search":
		q, _ := args["query"].(string)
		n := 5
		if v, ok := args["numResults"].(float64); ok && v > 0 {
			n = int(v)
		}
		return execWebSearch(q, n)
	case "webfetch":
		var urls []string
		if a, ok := args["urls"].([]any); ok {
			for _, u := range a {
				if s, ok := u.(string); ok && s != "" {
					urls = append(urls, s)
				}
			}
		}
		mc := 8000
		if v, ok := args["maxCharacters"].(float64); ok && v > 0 {
			mc = int(v)
		}
		return execWebFetch(urls, mc)
	}
	return "Unknown internal tool."
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	free := getFreeModels(false)
	data := make([]map[string]any, 0, len(free))
	for _, m := range free {
		id, _ := m["id"].(string)
		data = append(data, map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "opencode"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	model, _ := req["model"].(string)
	if model == "" {
		model = "deepseek-v4-flash-free"
	}
	instructions, _ := req["instructions"].(string)
	messages := parseInput(req["input"], instructions)
	upstreamTools := parseTools(toAnySlice(req["tools"]))
	allTools := append(internalTools(), upstreamTools...)

	respID := "resp_" + randHex()
	created := time.Now().Unix()
	baseResponse := map[string]any{
		"id": respID, "object": "response", "created_at": created, "status": "in_progress",
		"model": model, "output": []any{}, "usage": nil,
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush(w)

	sse(w, "response.created", baseResponse)
	sse(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": respID}})

	client := &http.Client{}

	streamRound := func() ([]map[string]any, map[string]any, []map[string]any, map[string]any, string, bool) {
		chatBody := map[string]any{
			"model":          model,
			"messages":       messages,
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
		}
		if len(allTools) > 0 {
			chatBody["tools"] = allTools
			chatBody["thinking"] = map[string]any{"type": "disabled"}
		}
		if mo, ok := req["max_output_tokens"]; ok {
			chatBody["max_tokens"] = mo
		}
		if temp, ok := req["temperature"]; ok {
			chatBody["temperature"] = temp
		}
		if top, ok := req["top_p"]; ok {
			chatBody["top_p"] = top
		}
		payload, _ := json.Marshal(chatBody)

		var upResp *http.Response
		var upErr error
		for attempt := 0; attempt < 3; attempt++ {
			upReq, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(payload))
			if err != nil {
				log.Printf("upstream request build error: %v", err)
				upErr = err
				break
			}
			upReq.Header.Set("Content-Type", "application/json")
			upReq.Header.Set("Accept", "text/event-stream")
			applyZenAuth(upReq)
			upResp, err = client.Do(upReq)
			if err != nil {
				log.Printf("upstream exception (attempt %d): %v", attempt+1, err)
				upErr = err
				if attempt < 2 {
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
					continue
				}
				break
			}
			// Retry transient upstream errors (429, 5xx); don't (re)build a non-OK check below.
			if upResp.StatusCode == http.StatusTooManyRequests || upResp.StatusCode >= 500 {
				transientStatus := upResp.StatusCode
				detail, _ := io.ReadAll(upResp.Body)
				upResp.Body.Close()
				log.Printf("upstream transient error %d (attempt %d): %s", transientStatus, attempt+1, string(detail))
				if attempt < 2 {
					time.Sleep(time.Duration(attempt+1) * 750 * time.Millisecond)
					continue
				}
				upResp = nil
				upErr = fmt.Errorf("upstream returned %d after retries: %s", transientStatus, string(detail))
			}
			break
		}
		if upErr != nil {
			sse(w, "response.failed", map[string]any{
				"type": "response.failed", "response": map[string]any{"id": respID},
				"error": map[string]any{"code": "upstream_error", "message": upErr.Error()},
			})
			return nil, nil, nil, nil, "", true
		}
		defer upResp.Body.Close()
		if upResp.StatusCode != http.StatusOK {
			detail, _ := io.ReadAll(upResp.Body)
			log.Printf("upstream error %d: %s", upResp.StatusCode, string(detail))
			sse(w, "response.failed", map[string]any{
				"type": "response.failed", "response": map[string]any{"id": respID},
				"error": map[string]any{"code": "upstream_http", "message": fmt.Sprintf("%d: %s", upResp.StatusCode, string(detail))},
			})
			return nil, nil, nil, nil, "", true
		}

		var messageItem map[string]any
		contentPartID := ""
		accContent := ""
		fcByIndex := map[int]map[string]any{}
		var finalUsage map[string]any
		stopReason := ""

		sc := bufio.NewReader(upResp.Body)

		var upstreamErr error
		for {
			line, readErr := sc.ReadString('\n')
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) != "" {
				line = strings.TrimSpace(line)
			}
			if !strings.HasPrefix(line, "data:") {
				if readErr != nil {
					upstreamErr = readErr
					break
				}
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			choices, _ := chunk["choices"].([]any)
			if len(choices) == 0 {
				if usage, ok := chunk["usage"].(map[string]any); ok {
					finalUsage = usage
				}
				continue
			}
			choice0, _ := choices[0].(map[string]any)
			delta, _ := choice0["delta"].(map[string]any)
			if fr, ok := choice0["finish_reason"].(string); ok && fr != "" {
				stopReason = fr
			}

			if content, ok := delta["content"].(string); ok && content != "" {
				if messageItem == nil {
					messageItem = map[string]any{
						"id": "msg_" + randHex(), "type": "message", "status": "in_progress",
						"role": "assistant", "content": []any{},
					}
					contentPartID = "part_" + randHex()
					sse(w, "response.output_item.added", map[string]any{
						"type": "response.output_item.added", "output_index": 0, "item": messageItem,
						"response": map[string]any{"id": respID},
					})
					sse(w, "response.content_part.added", map[string]any{
						"type": "response.content_part.added", "item_id": messageItem["id"],
						"output_index": 0, "content_index": 0,
						"part":     map[string]any{"id": contentPartID, "type": "output_text", "text": "", "annotations": []any{}},
						"response": map[string]any{"id": respID},
					})
				}
				accContent += content
				sse(w, "response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": messageItem["id"],
					"output_index": 0, "content_index": 0, "delta": content,
					"response": map[string]any{"id": respID},
				})
				continue
			}

			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					tc, _ := tcAny.(map[string]any)
					idxFloat, _ := tc["index"].(float64)
					idx := int(idxFloat)
					item := fcByIndex[idx]
					if item == nil {
						callID, _ := tc["id"].(string)
						if callID == "" {
							callID = "call_" + randHex()
						}
						var fname string
						if fn, ok := tc["function"].(map[string]any); ok {
							fname, _ = fn["name"].(string)
						}
						item = map[string]any{
							"id": callID, "type": "function_call", "status": "completed",
							"name": fname, "arguments": "", "call_id": callID,
							"response": map[string]any{"id": respID},
						}
						fcByIndex[idx] = item
						if !isInternalTool(fname) {
							sse(w, "response.output_item.added", map[string]any{
								"type": "response.output_item.added", "output_index": 0,
								"item":     map[string]any{"id": item["id"], "type": "function_call", "status": "in_progress", "name": item["name"], "arguments": ""},
								"response": map[string]any{"id": respID},
							})
						}
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok && n != "" {
							item["name"] = n
						}
						if args, ok := fn["arguments"].(string); ok && args != "" {
							item["arguments"] = item["arguments"].(string) + args
							if !isInternalTool(item["name"].(string)) {
								sse(w, "response.function_call_arguments.delta", map[string]any{
									"type": "response.function_call_arguments.delta", "item_id": item["id"],
									"output_index": 0, "delta": args,
									"response": map[string]any{"id": respID},
								})
							}
						}
					}
				}
				continue
			}
		}
		if upstreamErr != nil {
			log.Printf("upstream read error: %v", upstreamErr)
		}

		var outputItems []map[string]any
		if messageItem != nil {
			messageItem["status"] = "completed"
			messageItem["content"] = []any{map[string]any{"type": "output_text", "text": accContent, "annotations": []any{}}}
			sse(w, "response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": messageItem["id"],
				"output_index": 0, "content_index": 0, "text": accContent,
				"response": map[string]any{"id": respID},
			})
			sse(w, "response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": messageItem["id"],
				"output_index": 0, "content_index": 0,
				"part":     map[string]any{"id": contentPartID, "type": "output_text", "text": accContent, "annotations": []any{}},
				"response": map[string]any{"id": respID},
			})
			sse(w, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": 0, "item": messageItem,
				"response": map[string]any{"id": respID},
			})
			outputItems = append(outputItems, messageItem)
		}

		indices := make([]int, 0, len(fcByIndex))
		for i := range fcByIndex {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		var fcList []map[string]any
		for _, i := range indices {
			item := fcByIndex[i]
			if !isInternalTool(item["name"].(string)) {
				sse(w, "response.function_call_arguments.done", map[string]any{
					"type": "response.function_call_arguments.done", "item_id": item["id"],
					"output_index": 0, "arguments": item["arguments"],
					"response": map[string]any{"id": respID},
				})
				sse(w, "response.output_item.done", map[string]any{
					"type": "response.output_item.done", "output_index": 0, "item": item,
					"response": map[string]any{"id": respID},
				})
				outputItems = append(outputItems, item)
			}
			fcList = append(fcList, item)
		}

		var assistantMsg map[string]any
		if accContent != "" || len(fcList) > 0 {
			assistantMsg = map[string]any{"role": "assistant"}
			if accContent != "" {
				assistantMsg["content"] = accContent
			} else {
				assistantMsg["content"] = nil
			}
			if len(fcList) > 0 {
				var tcs []any
				for _, it := range fcList {
					tcs = append(tcs, map[string]any{
						"id": it["call_id"], "type": "function",
						"function": map[string]any{"name": it["name"], "arguments": it["arguments"]},
					})
				}
				assistantMsg["tool_calls"] = tcs
			}
		}
		return outputItems, assistantMsg, fcList, finalUsage, stopReason, false
	}

	var finalOutput []any
	var finalUsage map[string]any
	for round := 0; round < 12; round++ {
		outputItems, assistantMsg, fcList, usage, stopReason, failed := streamRound()
		if failed {
			return
		}
		if usage != nil {
			finalUsage = usage
		}
		if assistantMsg != nil {
			messages = append(messages, assistantMsg)
		}
		for _, it := range outputItems {
			finalOutput = append(finalOutput, it)
		}
		if len(fcList) == 0 {
			status := "completed"
			if stopReason == "length" {
				status = "incomplete"
			}
			completed := map[string]any{
				"id": respID, "object": "response", "created_at": created, "status": status,
				"model": model, "output": finalOutput, "usage": buildUsage(finalUsage),
			}
			sse(w, "response.completed", map[string]any{"type": "response.completed", "response": completed})
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush(w)
			return
		}
		anyInternal := false
		hasUpstream := false
		for _, fc := range fcList {
			name, _ := fc["name"].(string)
			if !isInternalTool(name) {
				hasUpstream = true
				continue
			}
			anyInternal = true
			argsJSON, _ := fc["arguments"].(string)

			// Keep the SSE connection alive (codex aborts a silent stream) while
			// the internal tool does its synchronous HTTP work.
			stopPing := make(chan struct{})
			go func() {
				t := time.NewTicker(2 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						fmt.Fprint(w, ": keepalive\n\n")
						flush(w)
					case <-stopPing:
						return
					}
				}
			}()
			result := execInternalTool(name, argsJSON)
			close(stopPing)

			log.Printf("internal tool %s -> %d bytes", name, len(result))
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": fc["call_id"], "content": result})
		}
		if !anyInternal || hasUpstream {
			completed := map[string]any{
				"id": respID, "object": "response", "created_at": created, "status": "completed",
				"model": model, "output": finalOutput, "usage": buildUsage(finalUsage),
			}
			sse(w, "response.completed", map[string]any{"type": "response.completed", "response": completed})
			fmt.Fprint(w, "data: [DONE]\n\n")
			flush(w)
			return
		}
	}
}

func buildUsage(u map[string]any) any {
	if u == nil {
		return nil
	}
	promptTokens, _ := u["prompt_tokens"].(float64)
	completionTokens, _ := u["completion_tokens"].(float64)
	total, _ := u["total_tokens"].(float64)
	return map[string]any{
		"input_tokens":          int(promptTokens),
		"output_tokens":         int(completionTokens),
		"total_tokens":          int(total),
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
}

func isInternalTool(name string) bool {
	return name == "web_search" || name == "webfetch"
}

func main() {
	if opencodeAPIKey == "" {
		log.Printf("%s not set; using keyless OpenCode Zen mode", apiKeyEnv)
	} else {
		log.Printf("using OpenCode API key from %s", apiKeyEnv)
	}
	getFreeModels(true)
	go modelsWatcher()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			modelsHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			responsesHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("opencode zen bridge listening on http://%s/v1/responses", listen)
	if err := http.ListenAndServe(listen, mux); err != nil {
		log.Fatal(err)
	}
}
