package irsclient

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"readiness.local/postgres/lifecycle"
)

const (
	intakePath       = "/IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance"
	statusPath       = "/IRIntakeAcceptanceA2A/1.0/iris/transstatusorack"
	maxResponseBytes = 1 << 20
)

type Config struct {
	BaseURL               string
	BearerToken           string
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	TotalTimeout          time.Duration
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type HTTPResult struct {
	StatusCode int
	ErrorCode  lifecycle.FailureCode
}

type SubmitResult struct {
	HTTPResult
	ReceiptID string
}

type SearchType string

const (
	SearchUTID      SearchType = "UTID"
	SearchReceiptID SearchType = "RECEIPTID"
)

type StatusResult struct {
	HTTPResult
	ReceiptID string
	UTID      string
	Status    string
	Outcomes  []lifecycle.FilingOutcome
}

type ProtocolError struct{ message string }

func (err *ProtocolError) Error() string { return err.message }

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.BaseURL) == "" || config.BearerToken == "" || config.ConnectTimeout <= 0 || config.ResponseHeaderTimeout <= 0 || config.TotalTimeout <= 0 {
		return nil, errors.New("invalid IRS client configuration")
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: config.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
	}
	return &Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"),
		token:   config.BearerToken,
		http:    &http.Client{Transport: transport, Timeout: config.TotalTimeout},
	}, nil
}

func NewWithHTTPClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || token == "" || httpClient == nil {
		return nil, errors.New("invalid IRS client configuration")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}, nil
}

func (client *Client) Submit(ctx context.Context, canonicalXML []byte) (SubmitResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="transmission.xml"`)
	header.Set("Content-Type", "application/xml")
	part, err := writer.CreatePart(header)
	if err != nil {
		return SubmitResult{}, err
	}
	if _, err := part.Write(canonicalXML); err != nil {
		return SubmitResult{}, err
	}
	if err := writer.Close(); err != nil {
		return SubmitResult{}, err
	}
	request, err := client.request(ctx, intakePath, writer.FormDataContentType(), &body)
	if err != nil {
		return SubmitResult{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return SubmitResult{}, err
	}
	defer response.Body.Close()
	payload, err := readResponse(response)
	if err != nil {
		return SubmitResult{}, err
	}
	result := SubmitResult{HTTPResult: HTTPResult{StatusCode: response.StatusCode}}
	if response.StatusCode == http.StatusOK {
		var receipt struct {
			XMLName xml.Name `xml:"ReceiptId"`
			Value   string   `xml:",chardata"`
		}
		if err := decodeStrict(payload, "ReceiptId", map[string]bool{"ReceiptId": true}, &receipt); err != nil || strings.TrimSpace(receipt.Value) == "" {
			return result, protocolError("invalid ReceiptId response", err)
		}
		result.ReceiptID = receipt.Value
		return result, nil
	}
	result.ErrorCode, err = decodeError(payload)
	return result, err
}

func (client *Client) Status(ctx context.Context, firmID string, searchType SearchType, searchValue string) (StatusResult, error) {
	if firmID == "" || searchValue == "" || (searchType != SearchUTID && searchType != SearchReceiptID) {
		return StatusResult{}, errors.New("invalid IRS status query")
	}
	requestPayload, err := xml.Marshal(statusRequest{FirmID: firmID, SearchType: string(searchType), SearchValue: searchValue})
	if err != nil {
		return StatusResult{}, err
	}
	request, err := client.request(ctx, statusPath, "application/xml", bytes.NewReader(requestPayload))
	if err != nil {
		return StatusResult{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return StatusResult{}, err
	}
	defer response.Body.Close()
	payload, err := readResponse(response)
	if err != nil {
		return StatusResult{}, err
	}
	result := StatusResult{HTTPResult: HTTPResult{StatusCode: response.StatusCode}}
	if response.StatusCode != http.StatusOK {
		result.ErrorCode, err = decodeError(payload)
		return result, err
	}
	var decoded statusResponse
	if err := decodeStrict(payload, "TransStatusOrAckResponse", statusResponseElements, &decoded); err != nil {
		return result, err
	}
	if decoded.ReceiptID == "" || decoded.UTID == "" || !validTransmissionStatus(decoded.Status) {
		return result, protocolError("incomplete IRS status response", nil)
	}
	if decoded.Status == "Processing" && len(decoded.Results) != 0 {
		return result, protocolError("processing response contains filing results", nil)
	}
	result.ReceiptID, result.UTID, result.Status = decoded.ReceiptID, decoded.UTID, decoded.Status
	for _, filing := range decoded.Results {
		accepted := filing.Status == "Accepted" && filing.RecordID != "" && filing.Reason == ""
		rejected := filing.Status == "Rejected" && filing.RecordID == "" && validReason(filing.Reason)
		if filing.FilingKey == "" || (!accepted && !rejected) {
			return StatusResult{}, protocolError("invalid per-filing IRS result", nil)
		}
		result.Outcomes = append(result.Outcomes, lifecycle.FilingOutcome{FilingKey: filing.FilingKey, IRSRecordID: filing.RecordID, RejectionReason: filing.Reason})
	}
	if decoded.Status != "Processing" && len(result.Outcomes) == 0 {
		return result, protocolError("completed IRS response has no filing results", nil)
	}
	return result, nil
}

func (client *Client) request(ctx context.Context, path, contentType string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("Content-Type", contentType)
	return request, nil
}

func readResponse(response *http.Response) ([]byte, error) {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/xml" {
		return nil, protocolError("IRS response is not application/xml", err)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxResponseBytes {
		return nil, protocolError("IRS response exceeds limit", nil)
	}
	return payload, nil
}

type statusRequest struct {
	XMLName     xml.Name `xml:"TransStatusOrAckRequest"`
	FirmID      string   `xml:"TransmitterControlCd"`
	SearchType  string   `xml:"SearchParameterTypeCd"`
	SearchValue string   `xml:"SearchParameterTxt"`
}

type statusResponse struct {
	XMLName   xml.Name     `xml:"TransStatusOrAckResponse"`
	ReceiptID string       `xml:"ReceiptId"`
	UTID      string       `xml:"UniqueTransmissionId"`
	Status    string       `xml:"TransmissionStatusCd"`
	Results   []wireResult `xml:"RecordResultGrp"`
}

type wireResult struct {
	FilingKey string                    `xml:"RecordId"`
	Status    string                    `xml:"RecordStatusCd"`
	RecordID  string                    `xml:"IRSRecordId"`
	Reason    lifecycle.PreflightReason `xml:"ErrorReasonCd"`
}

type errorResponse struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Code    string   `xml:"ErrorCd"`
	Message string   `xml:"ErrorMessageTxt"`
}

var statusResponseElements = map[string]bool{
	"TransStatusOrAckResponse": true, "ReceiptId": true, "UniqueTransmissionId": true,
	"TransmissionStatusCd": true, "RecordResultGrp": true, "RecordId": true,
	"RecordStatusCd": true, "IRSRecordId": true, "ErrorReasonCd": true,
}

func decodeError(payload []byte) (lifecycle.FailureCode, error) {
	var decoded errorResponse
	allowed := map[string]bool{"ErrorResponse": true, "ErrorCd": true, "ErrorMessageTxt": true}
	if err := decodeStrict(payload, "ErrorResponse", allowed, &decoded); err != nil || decoded.Code == "" {
		return "", protocolError("invalid IRS error response", err)
	}
	return lifecycle.FailureCode(decoded.Code), nil
}

func decodeStrict(payload []byte, root string, allowed map[string]bool, target any) error {
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return protocolError("invalid IRS XML", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != "" || len(value.Attr) != 0 || !allowed[value.Name.Local] {
				return protocolError("unknown IRS XML element", nil)
			}
			if depth == 0 {
				roots++
				if value.Name.Local != root {
					return protocolError("unexpected IRS XML root", nil)
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(value)) != "" {
				return protocolError("data outside IRS XML root", nil)
			}
		}
	}
	if roots != 1 || depth != 0 {
		return protocolError("incomplete IRS XML", nil)
	}
	if err := xml.Unmarshal(payload, target); err != nil {
		return protocolError("decode IRS XML", err)
	}
	return nil
}

func protocolError(message string, cause error) error {
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	return &ProtocolError{message: message}
}

func validTransmissionStatus(status string) bool {
	return status == "Processing" || status == "Accepted" || status == "PartiallyAccepted" || status == "Rejected"
}

func validReason(reason lifecycle.PreflightReason) bool {
	return reason == lifecycle.ReasonTINMissing || reason == lifecycle.ReasonTINMalformed || reason == lifecycle.ReasonTINInvalid || reason == lifecycle.ReasonAmountInvalid
}
