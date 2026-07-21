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

	// toolCallParametersRegex matches <parameters>...</parameters> or <parameter>...</parameter>
	toolCallParametersRegex = regexp.MustCompile(`(?i)<parameters>\s*(.*?)\s*</parameters>`)

	// toolCallJSONNameRegex finds occurrences of {"name": "tool_name" or {"name":"tool_name"
	toolCallJSONNameRegex = regexp.MustCompile(`(?i)\{"name"\s*:\s*["'](\w+)["']`)

	// toolCallBlockRegex matches <tool_call> ... </tool_call> blocks - using non-greedy but safe pattern
	toolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

	// toolCallJSONArgsRegex finds {"arguments": {...}} or {"input": {...}} - using non-greedy but safe pattern
	toolCallJSONArgsRegex = regexp.MustCompile(`(?i)"arguments"\s*:\s*(\{[^{}]*\})|"input"\s*:\s*(\{[^{}]*\})`)
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

	// If no tool calls found in <tool_call> blocks, try to find JSON tool call objects by looking for {"name": ...
	if len(toolCalls) == 0 {
		matches := toolCallJSONNameRegex.FindAllStringIndex(text, -1)
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
					// Try to parse as tool call
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

	// Check for <function>...</function> format
	funcMatches := toolCallFunctionRegex.FindAllStringSubmatch(blockContent, -1)
	if len(funcMatches) > 0 {
		for _, match := range funcMatches {
			if len(match) >= 2 {
				toolName := match[1]

				// Try to find parameters
				var argsMap map[string]interface{}
				var argsStr string

				paramMatches := toolCallParametersRegex.FindAllStringSubmatch(blockContent, -1)
				if len(paramMatches) > 0 {
					paramContent := paramMatches[0][1]
					// Try to parse as JSON
					var parsedArgs map[string]interface{}
					if err := json.Unmarshal([]byte(paramContent), &parsedArgs); err == nil {
						argsMap = parsedArgs
						argsStr = paramContent
					} else {
						// Try to extract JSON object from parameters
						jsonStr, _ := extractJSONObject(paramContent, 0)
						if jsonStr != "" {
							var parsedArgs map[string]interface{}
							if err := json.Unmarshal([]byte(jsonStr), &parsedArgs); err == nil {
								argsMap = parsedArgs
								argsStr = jsonStr
							} else {
								argsMap = map[string]interface{}{"input": paramContent}
								argsStr = paramContent
							}
						} else {
							argsMap = map[string]interface{}{"input": paramContent}
							argsStr = paramContent
						}
					}
				} else {
					// Try to find JSON arguments in the block content
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
							argsMap = parsedArgs
							argsStr = argsJSON
						} else {
							argsMap = map[string]interface{}{"input": argsJSON}
							argsStr = argsJSON
						}
					} else {
						argsMap = map[string]interface{}{}
						argsStr = "{}"
					}
				}

				toolCall := ToolCall{
					Index:   len(toolCalls),
					ID:      "tc_extracted_" + strconv.Itoa(len(toolCalls)),
					Name:    toolName,
					Args:    argsMap,
					ArgsStr: argsStr,
				}
				toolCalls = append(toolCalls, toolCall)
			}
		}
	} else {
		// Check for JSON format like {"name": "...", "arguments": {...}}
		matches := toolCallJSONNameRegex.FindAllStringSubmatch(blockContent, -1)
		if len(matches) > 0 {
			for _, match := range matches {
				if len(match) >= 2 {
					toolName := match[1]

					// Find the JSON object
					objStart := -1
					for i := 0; i < len(blockContent); i++ {
						if blockContent[i] == '{' {
							objStart = i
							break
						}
					}

					if objStart >= 0 {
						jsonStr, _ := extractJSONObject(blockContent, objStart)
						if jsonStr != "" {
							calls := parseToolCallFromJSONString(jsonStr)
							// Ensure the tool name is set if not already set by the JSON parser
							for i := range calls {
								if calls[i].Name == "" {
									calls[i].Name = sanitizeToolCallName(toolName)
								}
							}
							toolCalls = append(toolCalls, calls...)
						}
					}
				}
			}
		}
	}

	return toolCalls
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
		} else if c == '}' {
			depth--
			if depth == 0 {
				return src[start : i+1], i + 1
			}
		} else if c == '"' {
			// skip string
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
				} else if src[i] == '"' {
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

// sanitizeToolCallName removes any non-alphanumeric characters from tool call names
func sanitizeToolCallName(name string) string {
	var sb strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			sb.WriteRune(c)
		}
	}
	if sb.Len() == 0 {
		return "unknown_tool"
	}
	return sb.String()
}
