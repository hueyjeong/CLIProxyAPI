package cjkevi

import "encoding/json"

// TargetLanguage returns the request's target language and whether direct instructions conflict.
func TargetLanguage(request []byte, fallback string) (string, bool) {
	var input struct {
		Input    json.RawMessage   `json:"input"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(request, &input) == nil && len(input.Input) != 0 {
		var entries []json.RawMessage
		if json.Unmarshal(input.Input, &entries) == nil {
			for _, entry := range entries {
				var role struct {
					Role string `json:"role"`
				}
				if json.Unmarshal(entry, &role) == nil && (role.Role == "system" || role.Role == "developer") {
					input.Messages = append(input.Messages, entry)
				}
			}
			if len(input.Messages) != 0 {
				var payload map[string]json.RawMessage
				if json.Unmarshal(request, &payload) == nil {
					payload["messages"], _ = json.Marshal(input.Messages)
					if normalized, err := json.Marshal(payload); err == nil {
						request = normalized
					}
				}
			}
		}
	}
	language, _, _, conflict := requestLanguage(request, fallback)
	return language, conflict
}
