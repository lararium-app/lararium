package router

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInBandErrorClassification(t *testing.T) {
	parse := func(t *testing.T, body string) *inBandError {
		t.Helper()
		var r openAIResponse
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.Error == nil {
			t.Fatal("expected non-nil Error")
		}
		return r.Error
	}

	overloaded := parse(t, `{"error":{"message":"Upstream error from Nvidia: Service temporarily overloaded","code":503}}`)
	if !strings.Contains(overloaded.Error(), "HTTP 503") {
		t.Errorf("Error() = %q, want it to carry HTTP 503", overloaded.Error())
	}
	if !isRetryable(overloaded) {
		t.Errorf("503 in-band error must be retryable (failover/retry): %q", overloaded.Error())
	}

	rateLimited := parse(t, `{"error":{"message":"temporarily rate-limited","code":429}}`)
	if !isRetryable(rateLimited) {
		t.Errorf("429 in-band error must be retryable: %q", rateLimited.Error())
	}

	badReq := parse(t, `{"error":{"message":"invalid request","code":400}}`)
	if isRetryable(badReq) {
		t.Errorf("400 in-band error must NOT be retryable: %q", badReq.Error())
	}

	codeless := parse(t, `{"error":{"message":"boom"}}`)
	if codeless.Error() == "" || !strings.Contains(codeless.Error(), "boom") {
		t.Errorf("Error() = %q, want it to contain the message", codeless.Error())
	}
}
