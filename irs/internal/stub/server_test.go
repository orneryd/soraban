package stub

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testUTID = "550e8400-e29b-41d4-a716-446655440000:IRIS:F001::A"

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

type randomSequence struct {
	mu     sync.Mutex
	values []float64
	index  int
}

func (sequence *randomSequence) Next() float64 {
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	if sequence.index >= len(sequence.values) {
		return 0
	}
	value := sequence.values[sequence.index]
	sequence.index++
	return value
}

func TestServerSubmitAndAcknowledgeMixedResults(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	random := &randomSequence{values: []float64{0.99, 0.99, 0}}
	server := newTestServer(t, Config{
		Addr:        "127.0.0.1:0",
		BearerToken: "test-token",
	}, Dependencies{Now: clock.Now, RandomFloat64: random.Next})

	records := []testRecord{
		{id: "missing", tin: "", amount: "10.00"},
		{id: "malformed", tin: "12-3456789", amount: "10.00"},
		{id: "invalid", tin: "000123456", amount: "10.00"},
		{id: "amount", tin: "123456789", amount: "0.00"},
		{id: "accepted", tin: "987654321", amount: "600.00"},
	}
	intake := performIntake(t, server, testUTID, records)
	if intake.Code != http.StatusOK {
		t.Fatalf("intake status = %d, body = %s", intake.Code, intake.Body.String())
	}
	var receipt receiptIDXML
	decodeResponse(t, intake, &receipt)
	if receipt.Value == "" || strings.Contains(intake.Body.String(), "UniqueTransmissionId") {
		t.Fatalf("success body must contain only ReceiptId: %s", intake.Body.String())
	}

	processing := performStatus(t, server, "UTID", testUTID)
	var processingResponse statusResponseXML
	decodeResponse(t, processing, &processingResponse)
	if processingResponse.TransmissionStatus != "Processing" || len(processingResponse.RecordResults) != 0 {
		t.Fatalf("processing response = %#v", processingResponse)
	}

	clock.Advance(10 * time.Second)
	acknowledged := performStatus(t, server, "RECEIPTID", receipt.Value)
	var response statusResponseXML
	decodeResponse(t, acknowledged, &response)
	if response.TransmissionStatus != "PartiallyAccepted" || len(response.RecordResults) != len(records) {
		t.Fatalf("acknowledgment = %#v", response)
	}
	wantReasons := []string{"TIN_MISSING", "TIN_MALFORMED", "TIN_INVALID", "AMOUNT_INVALID", ""}
	for index, result := range response.RecordResults {
		if result.ErrorReason != wantReasons[index] {
			t.Errorf("result %d reason = %q, want %q", index, result.ErrorReason, wantReasons[index])
		}
	}
	accepted := response.RecordResults[len(response.RecordResults)-1]
	if accepted.Status != "Accepted" || accepted.IRSRecordID == "" {
		t.Fatalf("accepted result = %#v", accepted)
	}
}

func TestServerFailureBeforeRecord(t *testing.T) {
	server := newTestServer(t, Config{
		Addr:                    "127.0.0.1:0",
		BearerToken:             "test-token",
		FailBeforeRecordPercent: 100,
	}, Dependencies{RandomFloat64: func() float64 { return 0 }})

	response := performIntake(t, server, testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}})
	assertErrorCode(t, response, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	status := performStatus(t, server, "UTID", testUTID)
	assertErrorCode(t, status, http.StatusNotFound, "NOT_FOUND")
}

func TestDefaultFailureThresholds(t *testing.T) {
	tests := []struct {
		name string
		draw float64
		want intakeFault
	}{
		{name: "start of before-record range", draw: 0, want: failBeforeRecord},
		{name: "end of before-record range", draw: 6.9999, want: failBeforeRecord},
		{name: "start of after-record range", draw: 7, want: failAfterRecord},
		{name: "end of after-record range", draw: 11.9999, want: failAfterRecord},
		{name: "success range", draw: 12, want: intakeSucceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyIntakeFault(test.draw, 7, 5); got != test.want {
				t.Fatalf("classifyIntakeFault(%v, 7, 5) = %v, want %v", test.draw, got, test.want)
			}
		})
	}
}

func TestServerFailureAfterRecord(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	server := newTestServer(t, Config{
		Addr:                   "127.0.0.1:0",
		BearerToken:            "test-token",
		FailAfterRecordPercent: 100,
	}, Dependencies{Now: clock.Now, RandomFloat64: func() float64 { return 0 }})

	response := performIntake(t, server, testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}})
	assertErrorCode(t, response, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	status := performStatus(t, server, "UTID", testUTID)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), "<ReceiptId>") {
		t.Fatalf("status after recorded failure = %d, body = %s", status.Code, status.Body.String())
	}
}

func TestServerNeverAcknowledges(t *testing.T) {
	clock := &testClock{now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	server := newTestServer(t, Config{
		Addr:            "127.0.0.1:0",
		BearerToken:     "test-token",
		NeverAckPercent: 100,
	}, Dependencies{Now: clock.Now, RandomFloat64: func() float64 { return 0 }})

	response := performIntake(t, server, testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}})
	if response.Code != http.StatusOK {
		t.Fatalf("intake status = %d, body = %s", response.Code, response.Body.String())
	}
	clock.Advance(365 * 24 * time.Hour)
	status := performStatus(t, server, "UTID", testUTID)
	if !strings.Contains(status.Body.String(), "<TransmissionStatusCd>Processing</TransmissionStatusCd>") {
		t.Fatalf("never-ack status = %s", status.Body.String())
	}
}

func TestServerRejectsDuplicateUTIDAndRecord(t *testing.T) {
	server := newTestServer(t, Config{Addr: "127.0.0.1:0", BearerToken: "test-token"}, Dependencies{RandomFloat64: func() float64 { return 0.99 }})
	records := []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}}
	if first := performIntake(t, server, testUTID, records); first.Code != http.StatusOK {
		t.Fatalf("first intake status = %d, body = %s", first.Code, first.Body.String())
	}
	assertErrorCode(t, performIntake(t, server, testUTID, records), http.StatusConflict, "DUPLICATE_UTID")
	secondUTID := "550e8400-e29b-41d4-a716-446655440001:IRIS:F001::A"
	assertErrorCode(t, performIntake(t, server, secondUTID, records), http.StatusConflict, "DUPLICATE_RECORD")
}

func TestServerConcurrentDuplicateUTID(t *testing.T) {
	server := newTestServer(t, Config{Addr: "127.0.0.1:0", BearerToken: "test-token"}, Dependencies{RandomFloat64: func() float64 { return 0.99 }})
	const callers = 20
	var successes atomic.Int32
	var conflicts atomic.Int32
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for range callers {
		go func() {
			defer waitGroup.Done()
			response := performIntake(t, server, testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}})
			switch response.Code {
			case http.StatusOK:
				successes.Add(1)
			case http.StatusConflict:
				conflicts.Add(1)
			default:
				t.Errorf("concurrent intake status = %d, body = %s", response.Code, response.Body.String())
			}
		}()
	}
	waitGroup.Wait()
	if successes.Load() != 1 || conflicts.Load() != callers-1 {
		t.Fatalf("successes = %d, conflicts = %d", successes.Load(), conflicts.Load())
	}
}

func TestServerValidatesRequestsBeforeFaultInjection(t *testing.T) {
	server := newTestServer(t, Config{
		Addr:                    "127.0.0.1:0",
		BearerToken:             "test-token",
		FailBeforeRecordPercent: 100,
	}, Dependencies{RandomFloat64: func() float64 { return 0 }})

	unknownElement := strings.Replace(validTransmissionXML(testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}}), "</IRTransmission>", "<Unknown/></IRTransmission>", 1)
	assertErrorCode(t, performRawIntake(t, server, unknownElement, "text/xml"), http.StatusBadRequest, "INVALID_REQUEST")

	wrongMedia := performRawIntake(t, server, validTransmissionXML(testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}}), "application/json")
	assertErrorCode(t, wrongMedia, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")

	request := httptest.NewRequest(http.MethodPost, intakePath, strings.NewReader("not multipart"))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", "multipart/form-data")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertErrorCode(t, response, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestServerTransmissionValidation(t *testing.T) {
	server := newTestServer(t, Config{Addr: "127.0.0.1:0", BearerToken: "test-token"}, Dependencies{RandomFloat64: func() float64 { return 0.99 }})
	base := validTransmissionXML(testUTID, []testRecord{{id: "record-1", tin: "123456789", amount: "600.00"}})
	oneHundred := make([]testRecord, 100)
	for index := range oneHundred {
		oneHundred[index] = testRecord{id: fmt.Sprintf("record-%03d", index), tin: "123456789", amount: "1.00"}
	}
	if response := performIntake(t, server, "550e8400-e29b-41d4-a716-446655440099:IRIS:F001::A", oneHundred); response.Code != http.StatusOK {
		t.Fatalf("100-record intake status = %d, body = %s", response.Code, response.Body.String())
	}

	oneHundredOne := append(oneHundred, testRecord{id: "record-101", tin: "123456789", amount: "1.00"})
	tests := []struct {
		name    string
		payload string
	}{
		{name: "malformed XML", payload: strings.TrimSuffix(base, "</IRTransmission>")},
		{name: "invalid UTID", payload: strings.Replace(base, testUTID, "not-a-utid", 1)},
		{name: "wrong tax year", payload: strings.Replace(base, "<TaxYr>2025</TaxYr>", "<TaxYr>2024</TaxYr>", 1)},
		{name: "count mismatch", payload: strings.Replace(base, "<ReportedRcpntFormCnt>1</ReportedRcpntFormCnt>", "<ReportedRcpntFormCnt>2</ReportedRcpntFormCnt>", 1)},
		{name: "no records", payload: validTransmissionXML("550e8400-e29b-41d4-a716-446655440010:IRIS:F001::A", nil)},
		{name: "more than 100 records", payload: validTransmissionXML("550e8400-e29b-41d4-a716-446655440011:IRIS:F001::A", oneHundredOne)},
		{name: "duplicate record ID", payload: validTransmissionXML("550e8400-e29b-41d4-a716-446655440012:IRIS:F001::A", []testRecord{{id: "same", tin: "123456789", amount: "1.00"}, {id: "same", tin: "987654321", amount: "2.00"}})},
		{name: "inexact money", payload: strings.Replace(base, "600.00", "600.001", 1)},
		{name: "missing record ID", payload: strings.Replace(base, "record-1", "", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertErrorCode(t, performRawIntake(t, server, test.payload, "text/xml"), http.StatusBadRequest, "INVALID_REQUEST")
		})
	}
}

func TestServerHealthAndAuthentication(t *testing.T) {
	server := newTestServer(t, Config{Addr: "127.0.0.1:0", BearerToken: "test-token"}, Dependencies{})

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResponse := httptest.NewRecorder()
	server.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK || healthResponse.Body.String() != "ok\n" {
		t.Fatalf("health response = %d, %q", healthResponse.Code, healthResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, intakePath, nil)
	request.Header.Set("Accept", "application/xml")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	assertErrorCode(t, response, http.StatusUnauthorized, "UNAUTHORIZED")
}

func TestParseCents(t *testing.T) {
	tests := map[string]int64{
		"0.00":                 0,
		"600.00":               60000,
		"-1.25":                -125,
		"92233720368547758.07": math.MaxInt64,
	}
	for input, want := range tests {
		got, err := parseCents(input)
		if err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"1", "1.0", "1.000", "+1.00", "92233720368547758.08"} {
		if _, err := parseCents(input); err == nil {
			t.Errorf("parseCents(%q) error = nil", input)
		}
	}
}

func TestConfigValidateRejectsNaN(t *testing.T) {
	config := Config{Addr: "127.0.0.1:0", BearerToken: "test-token", NeverAckPercent: math.NaN()}
	if err := config.Validate(); err == nil {
		t.Fatal("Config.Validate() error = nil, want error")
	}
}

type testRecord struct {
	id     string
	tin    string
	amount string
}

func newTestServer(t *testing.T, config Config, dependencies Dependencies) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(config, dependencies, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func performIntake(t *testing.T, server http.Handler, utid string, records []testRecord) *httptest.ResponseRecorder {
	t.Helper()
	return performRawIntake(t, server, validTransmissionXML(utid, records), "text/xml")
}

func performRawIntake(t *testing.T, server http.Handler, payload, fileMediaType string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="file"; filename="transmission.xml"`}
	header["Content-Type"] = []string{fileMediaType}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("CreatePart() error = %v", err)
	}
	if _, err := io.WriteString(part, payload); err != nil {
		t.Fatalf("write multipart payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart payload: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, intakePath, &body)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func performStatus(t *testing.T, server http.Handler, searchType, searchText string) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(`<TransStatusOrAckRequest><TransmitterControlCd>F001</TransmitterControlCd><SearchParameterTypeCd>%s</SearchParameterTypeCd><SearchParameterTxt>%s</SearchParameterTxt></TransStatusOrAckRequest>`, searchType, searchText)
	request := httptest.NewRequest(http.MethodPost, statusPath, strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", "application/xml")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func validTransmissionXML(utid string, records []testRecord) string {
	var details strings.Builder
	for _, record := range records {
		fmt.Fprintf(&details, `<Form1099NECDetail><RecordId>%s</RecordId><RecipientNm>Vendor</RecipientNm><RecipientTIN>%s</RecipientTIN><NonemployeeCompensationAmt>%s</NonemployeeCompensationAmt><FederalIncomeTaxWithheldAmt>0.00</FederalIncomeTaxWithheldAmt></Form1099NECDetail>`, record.id, record.tin, record.amount)
	}
	return fmt.Sprintf(`<IRTransmission><IRTransmissionManifest><UniqueTransmissionId>%s</UniqueTransmissionId><TransmitterControlCd>F001</TransmitterControlCd><TransmissionTypeCd>O</TransmissionTypeCd><TaxYr>2025</TaxYr></IRTransmissionManifest><IRSubmission1Grp><IRSubmission1Header><SubmissionId>SUB-C0001-0001</SubmissionId><ClientId>C0001</ClientId><FormTypeCd>1099-NEC</FormTypeCd><ReportedRcpntFormCnt>%d</ReportedRcpntFormCnt></IRSubmission1Header>%s</IRSubmission1Grp></IRTransmission>`, utid, len(records), details.String())
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := xml.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func assertErrorCode(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, wantStatus, response.Body.String())
	}
	var errorResponse errorResponseXML
	decodeResponse(t, response, &errorResponse)
	if errorResponse.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body = %s", errorResponse.Code, wantCode, response.Body.String())
	}
}
