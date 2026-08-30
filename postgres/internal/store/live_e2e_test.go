package store_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"readiness/postgres/internal/app"
)

const (
	defaultIRSBaseURL = "http://127.0.0.1:8081"
	defaultIRSToken   = "local-irs-token"
	intakePath        = "/IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance"
	statusPath        = "/IRIntakeAcceptanceA2A/1.0/iris/transstatusorack"
)

type irisError struct {
	Code string `xml:"ErrorCd"`
}

type irisStatusRequest struct {
	XMLName     xml.Name `xml:"TransStatusOrAckRequest"`
	FirmID      string   `xml:"TransmitterControlCd"`
	SearchType  string   `xml:"SearchParameterTypeCd"`
	SearchValue string   `xml:"SearchParameterTxt"`
}

type irisStatusResponse struct {
	ReceiptID string `xml:"ReceiptId"`
	UTID      string `xml:"UniqueTransmissionId"`
	Status    string `xml:"TransmissionStatusCd"`
	Results   []struct {
		FilingKey string              `xml:"RecordId"`
		Status    string              `xml:"RecordStatusCd"`
		RecordID  string              `xml:"IRSRecordId"`
		Reason    app.PreflightReason `xml:"ErrorReasonCd"`
	} `xml:"RecordResultGrp"`
}

type irisHTTPResult struct {
	statusCode int
	receiptID  string
	errorCode  string
	status     irisStatusResponse
}

func liveIRSConfig() (string, string) {
	baseURL := os.Getenv("IRS_BASE_URL")
	if baseURL == "" {
		baseURL = defaultIRSBaseURL
	}
	token := os.Getenv("IRS_BEARER_TOKEN")
	if token == "" {
		token = defaultIRSToken
	}
	return strings.TrimRight(baseURL, "/"), token
}

func requireLiveIRS(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build IRS health request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("live IRS stub unavailable at %s: %v", baseURL, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64))
	if response.StatusCode != http.StatusOK || string(payload) != "ok\n" {
		t.Fatalf("IRS health response: status=%d body=%q", response.StatusCode, payload)
	}
}

func readIRISResponse(response *http.Response, target any) (string, error) {
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := xml.Unmarshal(payload, target); err != nil {
			return "", err
		}
		return "", nil
	}
	var failure irisError
	if err := xml.Unmarshal(payload, &failure); err != nil {
		return "", err
	}
	return failure.Code, nil
}

func submitTransmission(ctx context.Context, client *http.Client, baseURL, token string, payload []byte) (irisHTTPResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="transmission.xml"`)
	header.Set("Content-Type", "application/xml")
	part, err := writer.CreatePart(header)
	if err != nil {
		return irisHTTPResult{}, err
	}
	if _, err := part.Write(payload); err != nil {
		return irisHTTPResult{}, err
	}
	if err := writer.Close(); err != nil {
		return irisHTTPResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+intakePath, &body)
	if err != nil {
		return irisHTTPResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		return irisHTTPResult{}, err
	}
	defer response.Body.Close()
	result := irisHTTPResult{statusCode: response.StatusCode}
	var receipt struct {
		Value string `xml:",chardata"`
	}
	result.errorCode, err = readIRISResponse(response, &receipt)
	result.receiptID = receipt.Value
	return result, err
}

func retrieveStatus(ctx context.Context, client *http.Client, baseURL, token, firmID, searchType, searchValue string) (irisHTTPResult, error) {
	payload, err := xml.Marshal(irisStatusRequest{FirmID: firmID, SearchType: searchType, SearchValue: searchValue})
	if err != nil {
		return irisHTTPResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+statusPath, bytes.NewReader(payload))
	if err != nil {
		return irisHTTPResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", "application/xml")
	response, err := client.Do(request)
	if err != nil {
		return irisHTTPResult{}, err
	}
	defer response.Body.Close()
	result := irisHTTPResult{statusCode: response.StatusCode}
	result.errorCode, err = readIRISResponse(response, &result.status)
	return result, err
}

func finishPermit(t *testing.T, ctx context.Context, permit app.CallPermit, result irisHTTPResult, outcome app.CallOutcome) {
	t.Helper()
	if err := permit.Finish(ctx, app.CallResult{
		Outcome: outcome, HTTPStatus: result.statusCode, ErrorCode: app.FailureCode(result.errorCode),
	}); err != nil {
		t.Fatalf("finish rate permit: %v", err)
	}
}

func TestLiveLifecycle(t *testing.T) {
	if os.Getenv("RUN_LIVE_E2E") != "1" {
		t.Skip("set RUN_LIVE_E2E=1 to run the live IRS E2E test")
	}
	database := openStore(t)
	baseURL, token := liveIRSConfig()
	client := &http.Client{Timeout: 10 * time.Second}
	requireLiveIRS(t, client, baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	firmID := uniqueFirmID("LIVE")
	_, plan := prepareBatches(t, database, firmID, []app.PaymentRow{
		payment(1, "C-LIVE", "Live Vendor", "999999999", "600.00", "check", "0.00"),
	})
	if plan.CreatedBatchCount != 1 {
		t.Fatalf("created batches = %d, want 1", plan.CreatedBatchCount)
	}

	for step := 0; step < 30; step++ {
		work, found, err := database.ClaimNextBatch(ctx, app.ClaimBatchCommand{
			FirmID: firmID, WorkerID: fmt.Sprintf("live-worker-%d", step), LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("claim live work: %v", err)
		}
		if !found {
			t.Fatal("live workflow has no due work before acknowledgment")
		}

		operation := app.OperationStatus
		if work.NextAction == app.ActionSubmit {
			operation = app.OperationSubmit
		}
		permit, err := database.AcquireCallPermit(ctx, app.AcquireCallCommand{FirmID: firmID, BatchID: work.BatchID, Operation: operation})
		if err != nil {
			t.Fatalf("acquire live call permit: %v", err)
		}

		switch work.NextAction {
		case app.ActionSubmit:
			result, callErr := submitTransmission(ctx, client, baseURL, token, work.CanonicalXML)
			if callErr != nil {
				_ = permit.Finish(ctx, app.CallResult{Outcome: app.CallAmbiguous, ErrorCode: "TRANSPORT"})
				_ = permit.Close()
				if err := database.RecordSubmitUnknown(ctx, work.Claim, app.RetrySchedule{FailureCode: "TRANSPORT"}); err != nil {
					t.Fatalf("record transport ambiguity: %v", err)
				}
				continue
			}
			if result.statusCode == http.StatusOK {
				finishPermit(t, ctx, permit, result, app.CallCompleted)
				if err := database.RecordSubmitAccepted(ctx, work.Claim, app.SubmitAccepted{ReceiptID: result.receiptID}); err != nil {
					t.Fatalf("persist live receipt: %v", err)
				}
			} else if result.statusCode == http.StatusServiceUnavailable || (result.statusCode == http.StatusConflict && result.errorCode == "DUPLICATE_UTID") {
				outcome := app.CallAmbiguous
				if result.statusCode == http.StatusConflict {
					outcome = app.CallCompleted
				}
				finishPermit(t, ctx, permit, result, outcome)
				if err := database.RecordSubmitUnknown(ctx, work.Claim, app.RetrySchedule{FailureCode: app.FailureCode(result.errorCode)}); err != nil {
					t.Fatalf("persist live submit ambiguity: %v", err)
				}
			} else {
				finishPermit(t, ctx, permit, result, app.CallTerminalError)
				_ = database.FailBatchInvariant(ctx, work.Claim, app.FailureCode(result.errorCode))
				t.Fatalf("terminal IRS submit response: status=%d code=%s", result.statusCode, result.errorCode)
			}
			_ = permit.Close()

		case app.ActionLookupByUTID, app.ActionPollStatus:
			searchType, searchValue := "UTID", work.UTID
			if work.NextAction == app.ActionPollStatus && work.ReceiptID != "" {
				searchType, searchValue = "RECEIPTID", work.ReceiptID
			}
			result, callErr := retrieveStatus(ctx, client, baseURL, token, firmID, searchType, searchValue)
			if callErr != nil || result.statusCode >= 500 {
				callResult := app.CallResult{Outcome: app.CallRetryableError, ErrorCode: "STATUS_UNAVAILABLE"}
				if callErr == nil {
					callResult.HTTPStatus = result.statusCode
					callResult.ErrorCode = app.FailureCode(result.errorCode)
				}
				if err := permit.Finish(ctx, callResult); err != nil {
					t.Fatalf("finish unavailable status permit: %v", err)
				}
				_ = permit.Close()
				if err := database.RecordStatusUnavailable(ctx, work.Claim, app.RetrySchedule{FailureCode: callResult.ErrorCode}); err != nil {
					t.Fatalf("persist unavailable status: %v", err)
				}
				continue
			}
			finishPermit(t, ctx, permit, result, app.CallCompleted)
			_ = permit.Close()
			if result.statusCode == http.StatusNotFound && work.NextAction == app.ActionLookupByUTID {
				if err := database.RecordReferenceNotFound(ctx, work.Claim, app.RetrySchedule{FailureCode: "NOT_FOUND"}); err != nil {
					t.Fatalf("persist live reference absence: %v", err)
				}
				continue
			}
			if result.statusCode != http.StatusOK {
				_ = database.FailBatchInvariant(ctx, work.Claim, app.FailureCode(result.errorCode))
				t.Fatalf("terminal IRS status response: status=%d code=%s", result.statusCode, result.errorCode)
			}
			if work.NextAction == app.ActionLookupByUTID {
				if err := database.RecordReferenceFound(ctx, work.Claim, app.ReferenceFound{ReceiptID: result.status.ReceiptID}); err != nil {
					t.Fatalf("persist reconciled receipt: %v", err)
				}
				continue
			}
			if result.status.Status == "Processing" {
				if err := database.RecordAcknowledgmentPending(ctx, work.Claim, app.RetrySchedule{}); err != nil {
					t.Fatalf("persist pending acknowledgment: %v", err)
				}
				continue
			}
			outcomes := make([]app.FilingOutcome, 0, len(result.status.Results))
			for _, filing := range result.status.Results {
				outcomes = append(outcomes, app.FilingOutcome{
					FilingKey: filing.FilingKey, IRSRecordID: filing.RecordID, RejectionReason: filing.Reason,
				})
			}
			if err := database.CompleteAcknowledgment(ctx, work.Claim, outcomes); err != nil {
				t.Fatalf("persist live acknowledgment: %v", err)
			}
			status, err := database.ClientStatus(ctx, app.ClientStatusQuery{FirmID: firmID, ClientID: "C-LIVE"})
			if err != nil {
				t.Fatalf("read final live status: %v", err)
			}
			if status.Headline != app.HeadlineFullyFiled || status.Counts.Accepted != 1 {
				t.Fatalf("final live status = %+v, want one fully filed record", status)
			}
			return
		}
	}
	t.Fatal("live lifecycle did not acknowledge within 30 calls")
}
