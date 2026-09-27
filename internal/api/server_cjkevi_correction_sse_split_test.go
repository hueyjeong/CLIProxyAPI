package api

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
)

func TestCJKeviCorrectionSSESplitProseDeltas(t *testing.T) {
	message := strings.Replace(correctionMessageSSE, `"text":"中간 상황"`, `"text":"中간 "`, 1)
	message = strings.Replace(message, "event: content_block_stop", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"상황\"}}\n\nevent: content_block_stop", 1)
	responses := strings.Replace(correctionResponsesSSE, `"delta":"中간 상황"`, `"delta":"中간 "`, 1)
	responses = strings.Replace(responses, "event: response.output_text.done", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"상황\"}\n\nevent: response.output_text.done", 1)
	for _, test := range []struct{ name, surface, stream, cleared string }{
		{"Messages", cjkevi.SurfaceAnthropic, message, `"text":""`},
		{"Responses", cjkevi.SurfaceResponses, responses, `"delta":""`},
	} {
		t.Run(test.name, func(t *testing.T) {
			original, patch, ok := correctionSSEText(test.surface, []byte(test.stream))
			if !ok || original != correctionOriginal {
				t.Fatalf("split stream rejected: %q", original)
			}
			changed, err := patch(correctionRewritten)
			if err != nil || !bytes.Contains(changed, []byte(test.cleared)) {
				t.Fatalf("later delta not cleared: %v %s", err, changed)
			}
			actual, _, valid := correctionSSEText(test.surface, changed)
			if !valid || actual != correctionRewritten {
				t.Fatalf("incorrect re-extraction: %q", actual)
			}
		})
	}
}
