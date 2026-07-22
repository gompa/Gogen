package llm

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	// toolCallFunctionRegex matches <function>tool_name</function> or <function>tool_name</function>
	toolCallFunctionRegex = regexp.MustCompile(`(?i)<function>\s*(\w+)\s*</function>`)

	// toolCallFunctionEqRegex matches <function=tool_name> (equals-sign format used by some models)
	toolCallFunctionEqRegex = regexp.MustCompile(`(?i)<function\s*=\s*(\w+)\s*>`)

	// toolCallFunctionAttrRegex matches <function name="tool_name"> or <function name='tool_name'>
	toolCallFunctionAttrRegex = regexp.MustCompile(`(?i)<function\s+name\s*=\s*["'](\w+)["']\s*>`)

	// toolCallInvokeRegex matches Anthropic-style <invoke name="tool_name">
	toolCallInvokeRegex = regexp.MustCompile(`(?i)<invoke\s+name\s*=\s*["'](\w+)["']\s*>`)

	// toolCallToolNameRegex matches <tool_name>name</tool_name>
	toolCallToolNameRegex = regexp.MustCompile(`(?i)<tool_name>\s*(\w+)\s*</tool_name>`)

	// toolCallParamEqRegex matches <parameter=name>value</parameter> (equals-sign format)
	toolCallParamEqRegex = regexp.MustCompile(`(?si)<parameter\s*=\s*(\w+)\s*>\s*(.*?)\s*</parameter>`)

	// toolCallParamAttrRegex matches <parameter name="name">value</parameter> (attribute format,
	// including Anthropic-style)
	toolCallParamAttrRegex = regexp.MustCompile(`(?si)<parameter\s+name\s*=\s*["'](\w+)["']\s*>\s*(.*?)\s*</parameter>`)

	// toolCallParametersRegex matches <parameters>...</parameters> or <parameter>...</parameter>
	toolCallParametersRegex = regexp.MustCompile(`(?i)<parameters>\s*(.*?)\s*</parameters>`)

	// toolCallJSONNameRegex finds occurrences of {"name": "tool_name" or {"name":"tool_name"
	toolCallJSONNameRegex = regexp.MustCompile(`(?i)\{"name"\s*:\s*["'](\w+)["']`)

	// toolCallBlockRegex matches <tool_call> ... </tool_call> blocks - using non-greedy but safe pattern
	toolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

	// toolCallJSONArgsRegex finds {"arguments": {...}} or {"input": {...}} - using non-greedy but safe pattern
	toolCallJSONArgsRegex = regexp.MustCompile(`(?i)"arguments"\s*:\s*(\{[^{}]*\})|"input"\s*:\s*(\{[^{}]*\})`)

	// toolInvokeBlockRegex matches full <invoke ...> ... </invoke> blocks (Anthropic-style)
	toolInvokeBlockRegex = regexp.MustCompile(`(?s)<invoke\s[^>]*>.*?</invoke>`)
)

// extractToolCallsFromText scans text for embedded tool call patterns and returns them as ToolCall objects.
func extractToolCallsFromText(text string) []ToolCall {
	var toolCalls []ToolCall

	// First, try to find <tool_call> ... </tool_call> blocks
	blockMatches := toolCallBlockRegex.FindAllStringSubmatch(text, -1)
	for _, match := range blockMatches {
		if len(match) >= 2 {
			blockContent := match[1]
			calls := extractToolCallsFromBlock(blockContent)
			toolCalls = append(toolCalls, calls...)
		}
	}

	// Also try <invoke> blocks (Anthropic-style, may appear outside <tool_call>)
	invokeMatches := toolInvokeBlockRegex.FindAllString(text, -1)
	for _, fullMatch := range invokeMatches {
		calls := extractToolCallsFromBlock(fullMatch)
		toolCalls = append(toolCalls, calls...)
	}

	// If no tool calls found yet, try to find JSON tool call objects by looking for {"name": ...
	if len(toolCalls) == 0 {
		matches := toolCallJSONNameRegex.FindAllStringIndex(text, -1)
		seenEnd := make(map[int]struct{})
		for _, match := range matches {
			// match[0] is the start of the match, which is '{' in {"name": ...
			startIdx := match[0]

			// Find the nearest '{' at or before startIdx
			objStart := -1
			for i := startIdx; i >= 0; i-- {
				if text[i] == '{' {
					objStart = i
					break
				} else if text[i] == '"' {
					// We hit a string quote without finding '{', stop looking back
					break
				}
			}

			if objStart >= 0 {
				// Extract JSON object
				jsonStr, endIdx := extractJSONObject(text, objStart)
				if jsonStr != "" && endIdx > objStart {
					if _, ok := seenEnd[endIdx]; ok {
						continue
					}
					seenEnd[endIdx] = struct{}{}
					calls := parseToolCallFromJSONString(jsonStr)
					if len(calls) > 0 {
						toolCalls = append(toolCalls, calls...)
					}
				}
			}
		}
	}

	return toolCalls
}

// extractToolCallsFromBlock extracts tool calls from a <tool_call> ... </tool_call> block content
func extractToolCallsFromBlock(blockContent string) []ToolCall {
	var toolCalls []ToolCall

	// Strategy 1: Try to find JSON tool call objects inside the block
	// (e.g. <tool_call>{"name": "...", "arguments": {...}}</tool_call>)
	// FindAllStringIndex so each match can locate its own object; scanning from
	// byte 0 on every match would re-parse the first object repeatedly.
	jsonMatches := toolCallJSONNameRegex.FindAllStringIndex(blockContent, -1)
	if len(jsonMatches) > 0 {
		seenEnd := make(map[int]struct{})
		for _, loc := range jsonMatches {
			startIdx := loc[0]
			objStart := -1
			for i := startIdx; i >= 0; i-- {
				if blockContent[i] == '{' {
					objStart = i
					break
				} else if blockContent[i] == '"' {
					break
				}
			}
			if objStart < 0 {
				continue
			}
			jsonStr, endIdx := extractJSONObject(blockContent, objStart)
			if jsonStr == "" || endIdx <= objStart {
				continue
			}
			if _, ok := seenEnd[endIdx]; ok {
				continue
			}
			seenEnd[endIdx] = struct{}{}
			calls := parseToolCallFromJSONString(jsonStr)
			for i := range calls {
				calls[i].Index = len(toolCalls)
				calls[i].ID = "tc_extracted_" + strconv.Itoa(len(toolCalls))
			}
			toolCalls = append(toolCalls, calls...)
		}
		if len(toolCalls) > 0 {
			return toolCalls
		}
	}

	// Strategy 2: Try XML-based formats
	toolName, argsMap, argsStr := extractXMLToolCall(blockContent)
	if toolName != "" {
		toolCalls = append(toolCalls, ToolCall{
			Index:   len(toolCalls),
			ID:      "tc_extracted_" + strconv.Itoa(len(toolCalls)),
			Name:    toolName,
			Args:    argsMap,
			ArgsStr: argsStr,
		})
	}

	return toolCalls
}

// extractXMLToolCall tries multiple XML-based tool call formats and returns
// the tool name, parsed args, and raw args string. Returns empty name if no
// format matches.
func extractXMLToolCall(blockContent string) (string, map[string]interface{}, string) {
	// Try each function-name extraction pattern (ordered most-specific first)
	funcPatterns := []struct {
		re   *regexp.Regexp
		name string // description for debugging
	}{
		{toolCallFunctionAttrRegex, "function name=attr"},
		{toolCallFunctionEqRegex, "function=name"},
		{toolCallFunctionRegex, "function>name<"},
		{toolCallInvokeRegex, "invoke name=attr"},
		{toolCallToolNameRegex, "tool_name"},
	}

	for _, fp := range funcPatterns {
		funcMatches := fp.re.FindAllStringSubmatch(blockContent, -1)
		for _, match := range funcMatches {
			if len(match) >= 2 {
				toolName := match[1]

				// Try <parameters>JSON</parameters> first (most common)
				if paramMatches := toolCallParametersRegex.FindAllStringSubmatch(blockContent, -1); len(paramMatches) > 0 {
					paramContent := paramMatches[0][1]
					argsMap, argsStr := parseParamContent(paramContent)
					return toolName, argsMap, argsStr
				}

				// Try individual <parameter=name>value</parameter> (equals-sign format)
				if paramEqMatches := toolCallParamEqRegex.FindAllStringSubmatch(blockContent, -1); len(paramEqMatches) > 0 {
					argsMap := make(map[string]interface{})
					for _, pm := range paramEqMatches {
						if len(pm) >= 3 {
							pName := pm[1]
							pValue := strings.TrimSpace(pm[2])
							argsMap[pName] = parseParamValue(pValue)
						}
					}
					argsJSON, _ := json.Marshal(argsMap)
					return toolName, argsMap, string(argsJSON)
				}

				// Try individual <parameter name="name">value</parameter> (attribute format)
				if paramAttrMatches := toolCallParamAttrRegex.FindAllStringSubmatch(blockContent, -1); len(paramAttrMatches) > 0 {
					argsMap := make(map[string]interface{})
					for _, pm := range paramAttrMatches {
						if len(pm) >= 3 {
							pName := pm[1]
							pValue := strings.TrimSpace(pm[2])
							argsMap[pName] = parseParamValue(pValue)
						}
					}
					argsJSON, _ := json.Marshal(argsMap)
					return toolName, argsMap, string(argsJSON)
				}

				// Try JSON arguments in the block content
				argsMatches := toolCallJSONArgsRegex.FindStringSubmatch(blockContent)
				if len(argsMatches) >= 2 {
					var argsJSON string
					if argsMatches[1] != "" {
						argsJSON = argsMatches[1]
					} else if argsMatches[2] != "" {
						argsJSON = argsMatches[2]
					}
					var parsedArgs map[string]interface{}
					if err := json.Unmarshal([]byte(argsJSON), &parsedArgs); err == nil {
						return toolName, parsedArgs, argsJSON
					}
					// Fallback: wrap as input
					return toolName, map[string]interface{}{"input": argsJSON}, argsJSON
				}

				// No parameters found — return empty args
				return toolName, map[string]interface{}{}, "{}"
			}
		}
	}

	return "", nil, ""
}

// parseParamContent parses the content of a <parameters> tag, trying JSON first.
func parseParamContent(paramContent string) (map[string]interface{}, string) {
	// Try to parse as JSON
	var parsedArgs map[string]interface{}
	if err := json.Unmarshal([]byte(paramContent), &parsedArgs); err == nil {
		return parsedArgs, paramContent
	}
	// Try to extract JSON object from the parameter content
	jsonStr, _ := extractJSONObject(paramContent, 0)
	if jsonStr != "" {
		var inner map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &inner); err == nil {
			return inner, jsonStr
		}
	}
	return map[string]interface{}{"input": paramContent}, paramContent
}

// parseParamValue tries to interpret a parameter value as a typed Go value.
func parseParamValue(v string) interface{} {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	case "null", "none", "nil":
		return nil
	}
	// Try integer
	if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
		if strconv.FormatInt(iv, 10) == v {
			return float64(iv)
		}
	}
	// Try float
	if fv, err := strconv.ParseFloat(v, 64); err == nil {
		return fv
	}
	return v
}

// extractJSONObject extracts a JSON object starting at src[start:]
// It returns the JSON string and the end index (exclusive)
func extractJSONObject(src string, start int) (string, int) {
	if start >= len(src) || src[start] != '{' {
		return "", -1
	}
	depth := 0
	i := start
	for i < len(src) {
		c := src[i]
		if c == '{' {
			depth++
			i++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return src[start : i+1], i + 1
			}
			i++
		} else if c == '"' {
			// skip string
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
				} else if src[i] == '"' {
					i++ // advance past the closing quote
					break
				} else {
					i++
				}
			}
		} else {
			i++
		}
	}
	return "", -1
}

func parseToolCallFromJSONString(jsonStr string) []ToolCall {
	var result []ToolCall

	// Try to unmarshal as a map
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
		return result
	}

	name := ""
	if n, ok := obj["name"].(string); ok && n != "" {
		name = n
	} else if n, ok := obj["function"].(string); ok && n != "" {
		name = n
	} else if n, ok := obj["function"].(map[string]interface{}); ok {
		if nName, ok := n["name"].(string); ok && nName != "" {
			name = nName
		}
	}

	if name == "" {
		return result
	}

	arguments := obj["arguments"]
	if arguments == nil {
		// check if 'input' or the whole obj is the args
		if _, ok := obj["input"]; ok {
			arguments = obj["input"]
		} else {
			// try to find any other key that is an object or string
			for k, v := range obj {
				if k == "name" || k == "function" || k == "id" || k == "type" {
					continue
				}
				arguments = v
				break
			}
		}
	}

	argsMap := make(map[string]interface{})
	argsStr := ""

	if argsObj, ok := arguments.(map[string]interface{}); ok {
		argsMap = argsObj
		argsJSON, err := json.Marshal(argsObj)
		if err == nil {
			argsStr = string(argsJSON)
		}
	} else if argsStrVal, ok := arguments.(string); ok {
		// try to parse string as JSON
		var parsedArgs map[string]interface{}
		if err := json.Unmarshal([]byte(argsStrVal), &parsedArgs); err == nil {
			argsMap = parsedArgs
			argsStr = argsStrVal
		} else {
			argsMap = map[string]interface{}{"input": argsStrVal}
			argsStr = argsStrVal
		}
	} else if arguments != nil {
		// fallback: marshal the arguments
		argsJSON, err := json.Marshal(arguments)
		if err == nil {
			argsStr = string(argsJSON)
			var parsedArgs map[string]interface{}
			if json.Unmarshal(argsJSON, &parsedArgs) == nil {
				argsMap = parsedArgs
			} else {
				argsMap = map[string]interface{}{"input": arguments}
			}
		} else {
			argsMap = map[string]interface{}{"input": arguments}
		}
	}

	// Create ToolCall
	toolCall := ToolCall{
		Index:   len(result),
		ID:      "tc_extracted_" + strconv.Itoa(len(result)),
		Name:    name,
		Args:    argsMap,
		ArgsStr: argsStr,
	}
	result = append(result, toolCall)

	return result
}
