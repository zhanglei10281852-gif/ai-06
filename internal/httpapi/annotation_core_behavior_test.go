package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/zhanglei10281852-gif/ai/internal/domain"
	"github.com/zhanglei10281852-gif/ai/internal/repository"
)

func TestAuthenticatedAuditKeepsRequestID(t *testing.T) {
	f := newHTTPFixture(t)
	login := f.request(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email": "ops@example.test", "password": "very-secure-password",
	}, "")
	loginBody := readResponse(t, login)
	token, _ := loginBody["token"].(string)
	if token == "" {
		t.Fatalf("login body = %+v", loginBody)
	}

	payload, err := json.Marshal(map[string]any{
		"code": "REQUEST-AUDIT", "name": "Request audit workspace",
		"minimum_score": 0.8, "maximum_score": 0.99,
		"max_execution_hours": 12, "review_hours": 4, "business_timezone": "Asia/Shanghai",
	})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "request-authenticated-audit"
	request, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/workspaces", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		defer response.Body.Close()
		t.Fatalf("create workspace status = %d", response.StatusCode)
	}
	var workspace domain.Workspace
	if err := json.NewDecoder(response.Body).Decode(&workspace); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get("X-Request-ID") != requestID {
		t.Fatalf("response request ID = %q", response.Header.Get("X-Request-ID"))
	}

	auditPath := "/api/v1/audit?request_id=" + url.QueryEscape(requestID) + "&entity_type=workspace&entity_id=" + url.QueryEscape(workspace.ID)
	auditResponse := f.request(t, http.MethodGet, auditPath, nil, token)
	defer auditResponse.Body.Close()
	if auditResponse.StatusCode != http.StatusOK {
		t.Fatalf("audit status = %d", auditResponse.StatusCode)
	}
	var page repository.AuditPage
	if err := json.NewDecoder(auditResponse.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("audit page = %+v", page)
	}
	if page.Items[0].RequestID != requestID || page.Items[0].EntityID != workspace.ID {
		t.Fatalf("workspace audit event = %+v", page.Items[0])
	}
}
