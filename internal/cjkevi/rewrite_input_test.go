package cjkevi

import "testing"

func TestTargetLanguageResponsesInputAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		request  string
		language string
		conflict bool
	}{
		{"input system and user conflict", `{"input":[{"role":"system","content":"Reply in Korean"},{"role":"user","content":"Reply in Japanese"}]}`, "ko-KR", true},
		{"input developer and user conflict", `{"input":[{"role":"developer","content":"Reply in Korean"},{"role":"user","content":"Reply in Japanese"}]}`, "ko-KR", true},
		{"input system selects language", `{"input":[{"role":"system","content":"Reply in Japanese"},{"role":"user","content":"Hello"}]}`, "ja-JP", false},
		{"input system and top-level system conflict", `{"instructions":"Reply in Korean","input":[{"role":"system","content":"Reply in Japanese"}]}`, "ko-KR", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			language, conflict := TargetLanguage([]byte(test.request), "ko-KR")
			if language != test.language || conflict != test.conflict {
				t.Fatalf("language=%q conflict=%v; want %q, %v", language, conflict, test.language, test.conflict)
			}
		})
	}
}
