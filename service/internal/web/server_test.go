package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"readiness.local/postgres/lifecycle"
)

type fakeApplication struct{ firmStatusCalls int }

func (fake *fakeApplication) Import(context.Context, string, int, string) (lifecycle.DatasetResult, error) {
	return lifecycle.DatasetResult{DatasetID: 3, RowCount: 10}, nil
}
func (fake *fakeApplication) DetermineAndPlan(context.Context, string, int64) (lifecycle.DeterminationResult, lifecycle.BatchPlanResult, error) {
	return lifecycle.DeterminationResult{DeterminationID: 4}, lifecycle.BatchPlanResult{CreatedBatchCount: 2}, nil
}
func (fake *fakeApplication) FirmStatus(_ context.Context, firmID string) (lifecycle.FirmStatus, error) {
	fake.firmStatusCalls++
	return lifecycle.FirmStatus{FirmID: firmID, Headline: lifecycle.HeadlinePartiallyFiled, Counts: lifecycle.StatusCounts{Required: 2, Ready: 2}}, nil
}
func (fake *fakeApplication) ClientStatus(_ context.Context, firmID, clientID string) (lifecycle.ClientStatus, error) {
	return lifecycle.ClientStatus{FirmID: firmID, ClientID: clientID, Headline: lifecycle.HeadlineNeedsAttention, Counts: lifecycle.StatusCounts{Required: 1, Blocked: 1}}, nil
}
func (fake *fakeApplication) Exceptions(context.Context, string, string) ([]lifecycle.ExceptionGroup, error) {
	return []lifecycle.ExceptionGroup{{Type: "MISSING_TIN", Count: 1}}, nil
}

func TestHomeQueriesLifecycleEveryRequest(t *testing.T) {
	fake := &fakeApplication{}
	server, err := New(fake, []string{"F001"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "F001") {
			t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
		}
	}
	if fake.firmStatusCalls != 2 {
		t.Fatalf("status calls=%d, want 2", fake.firmStatusCalls)
	}
}

func TestActionsInvokeApplication(t *testing.T) {
	server, _ := New(&fakeApplication{}, []string{"F001"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/actions/import", strings.NewReader("firm_id=F001&tax_year=2025&input=data%2Ffirm_F001_export.csv.gz"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("import response=%d", response.Code)
	}
}
