package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
)

// deployBodyFields are the keys deploy.DeployRequest accepts. encoding/json
// matches field names case-insensitively, so the unknown-field check below
// compares the same way.
var deployBodyFields = []string{"alias", "network", "notes", "dry_run", "git_commit"}

// hasUnknownTopLevelField reports whether body is a JSON object carrying a key
// DeployRequest does not declare. Bodies that are not objects are left to the
// decoder to reject.
func hasUnknownTopLevelField(body []byte) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	for key := range obj {
		known := false
		for _, field := range deployBodyFields {
			if strings.EqualFold(key, field) {
				known = true
				break
			}
		}
		if !known {
			return true
		}
	}
	return false
}

// FuzzDeployBody pins three properties of request decoding on POST /v1/deploy,
// for arbitrary bytes: it never panics, it always answers 2xx or a 4xx with a
// JSON error body, and a body with an unknown top-level field never reaches the
// service.
func FuzzDeployBody(f *testing.F) {
	f.Add([]byte(`{"alias":"counter"}`))
	f.Add([]byte(`{"alias":"counter","dry_run":true}`))
	f.Add([]byte(`{"alias":"counter","dryrun":true}`))
	f.Add([]byte(`{"alias":"counter","network":"testnet","notes":"n","git_commit":"abc"}`))
	f.Add([]byte(`{"ALIAS":"counter"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"alias":`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		svc := &fakeService{deployResult: &deploy.DeployResult{Alias: "counter"}}
		router := newTestRouter(t, svc)

		req := httptest.NewRequest(http.MethodPost, "/v1/deploy", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		code := rec.Code
		switch {
		case code >= 200 && code < 300:
		case code >= 400 && code < 500:
			var payload struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Error == "" {
				t.Fatalf("status %d without a JSON error body: %q", code, rec.Body.String())
			}
		default:
			t.Fatalf("unexpected status %d for body %q", code, body)
		}

		if hasUnknownTopLevelField(body) && svc.calls != 0 {
			t.Fatalf("body with an unknown field reached the service: %q", body)
		}
	})
}
