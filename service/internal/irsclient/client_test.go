package irsclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubmitPreservesCanonicalXML(t *testing.T) {
	payload := []byte("<IRTransmission><value>exact</value></IRTransmission>")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != intakePath || request.Header.Get("Authorization") != "Bearer token" || request.Header.Get("Accept") != "application/xml" {
			t.Errorf("unexpected request metadata")
		}
		parts, err := request.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			return
		}
		file, err := parts.NextPart()
		if err != nil {
			t.Errorf("multipart file: %v", err)
			return
		}
		got, _ := io.ReadAll(file)
		if string(got) != string(payload) {
			t.Errorf("payload changed: %q", got)
		}
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(writer, "<ReceiptId>receipt-1</ReceiptId>")
	}))
	defer server.Close()
	client, err := NewWithHTTPClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Submit(context.Background(), payload)
	if err != nil || result.ReceiptID != "receipt-1" {
		t.Fatalf("submit = %+v, %v", result, err)
	}
}

func TestStatusStrictlyDecodesResultsAndErrors(t *testing.T) {
	response := `<TransStatusOrAckResponse><ReceiptId>r1</ReceiptId><UniqueTransmissionId>u1</UniqueTransmissionId><TransmissionStatusCd>PartiallyAccepted</TransmissionStatusCd><RecordResultGrp><RecordId>k1</RecordId><RecordStatusCd>Accepted</RecordStatusCd><IRSRecordId>i1</IRSRecordId></RecordResultGrp><RecordResultGrp><RecordId>k2</RecordId><RecordStatusCd>Rejected</RecordStatusCd><ErrorReasonCd>TIN_INVALID</ErrorReasonCd></RecordResultGrp></TransStatusOrAckResponse>`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(writer, response)
	}))
	defer server.Close()
	client, _ := NewWithHTTPClient(server.URL, "token", server.Client())
	result, err := client.Status(context.Background(), "F001", SearchUTID, "u1")
	if err != nil || result.Status != "PartiallyAccepted" || len(result.Outcomes) != 2 {
		t.Fatalf("status = %+v, %v", result, err)
	}

	response = `<TransStatusOrAckResponse><ReceiptId>r1</ReceiptId><UniqueTransmissionId>u1</UniqueTransmissionId><TransmissionStatusCd>Processing</TransmissionStatusCd><Unknown>x</Unknown></TransStatusOrAckResponse>`
	if _, err := client.Status(context.Background(), "F001", SearchUTID, "u1"); err == nil {
		t.Fatal("unknown XML element accepted")
	}
}

func TestResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(writer, strings.Repeat("x", maxResponseBytes+1))
	}))
	defer server.Close()
	client, _ := NewWithHTTPClient(server.URL, "token", &http.Client{Timeout: time.Second})
	if _, err := client.Status(context.Background(), "F001", SearchUTID, "u1"); err == nil {
		t.Fatal("oversized response accepted")
	}
}
