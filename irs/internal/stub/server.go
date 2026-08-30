package stub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	intakePath = "/IRIntakeAcceptanceA2A/1.0/irisa2a/v1/intake-acceptance"
	statusPath = "/IRIntakeAcceptanceA2A/1.0/iris/transstatusorack"
)

var moneyPattern = regexp.MustCompile(`^-?[0-9]+\.[0-9]{2}$`)

var errUnsupportedMediaType = errors.New("unsupported media type")

type Server struct {
	config          Config
	store           *Store
	logger          *slog.Logger
	mux             *http.ServeMux
	requestSequence atomic.Uint64
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(payload)
}

type transmissionXML struct {
	XMLName     xml.Name        `xml:"IRTransmission"`
	Manifests   []manifestXML   `xml:"IRTransmissionManifest"`
	Submissions []submissionXML `xml:"IRSubmission1Grp"`
}

type manifestXML struct {
	UniqueTransmissionID string `xml:"UniqueTransmissionId"`
	TransmitterControlCD string `xml:"TransmitterControlCd"`
	TransmissionTypeCD   string `xml:"TransmissionTypeCd"`
	TaxYear              string `xml:"TaxYr"`
}

type submissionXML struct {
	Headers []submissionHeaderXML `xml:"IRSubmission1Header"`
	Records []filingRecordXML     `xml:"Form1099NECDetail"`
}

type submissionHeaderXML struct {
	SubmissionID         string `xml:"SubmissionId"`
	ClientID             string `xml:"ClientId"`
	FormTypeCD           string `xml:"FormTypeCd"`
	ReportedRecipientCnt int    `xml:"ReportedRcpntFormCnt"`
}

type filingRecordXML struct {
	RecordID                       string `xml:"RecordId"`
	RecipientName                  string `xml:"RecipientNm"`
	RecipientTIN                   string `xml:"RecipientTIN"`
	NonemployeeCompensationAmount  string `xml:"NonemployeeCompensationAmt"`
	FederalIncomeTaxWithheldAmount string `xml:"FederalIncomeTaxWithheldAmt"`
}

type statusRequestXML struct {
	XMLName              xml.Name `xml:"TransStatusOrAckRequest"`
	TransmitterControlCD string   `xml:"TransmitterControlCd"`
	SearchParameterType  string   `xml:"SearchParameterTypeCd"`
	SearchParameterText  string   `xml:"SearchParameterTxt"`
}

type receiptIDXML struct {
	XMLName xml.Name `xml:"ReceiptId"`
	Value   string   `xml:",chardata"`
}

type statusResponseXML struct {
	XMLName              xml.Name          `xml:"TransStatusOrAckResponse"`
	ReceiptID            string            `xml:"ReceiptId"`
	UniqueTransmissionID string            `xml:"UniqueTransmissionId"`
	TransmissionStatus   string            `xml:"TransmissionStatusCd"`
	RecordResults        []recordResultXML `xml:"RecordResultGrp,omitempty"`
}

type recordResultXML struct {
	RecordID    string `xml:"RecordId"`
	Status      string `xml:"RecordStatusCd"`
	IRSRecordID string `xml:"IRSRecordId,omitempty"`
	ErrorReason string `xml:"ErrorReasonCd,omitempty"`
}

type errorResponseXML struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Code    string   `xml:"ErrorCd"`
	Message string   `xml:"ErrorMessageTxt"`
}

var intakeElements = map[string]struct{}{
	"IRTransmission": {}, "IRTransmissionManifest": {}, "UniqueTransmissionId": {},
	"TransmitterControlCd": {}, "TransmissionTypeCd": {}, "TaxYr": {},
	"IRSubmission1Grp": {}, "IRSubmission1Header": {}, "SubmissionId": {},
	"ClientId": {}, "FormTypeCd": {}, "ReportedRcpntFormCnt": {},
	"Form1099NECDetail": {}, "RecordId": {}, "RecipientNm": {}, "RecipientTIN": {},
	"NonemployeeCompensationAmt": {}, "FederalIncomeTaxWithheldAmt": {},
}

var statusElements = map[string]struct{}{
	"TransStatusOrAckRequest": {}, "TransmitterControlCd": {},
	"SearchParameterTypeCd": {}, "SearchParameterTxt": {},
}

func NewServer(config Config, dependencies Dependencies, logger *slog.Logger) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		config: config,
		store:  newStore(config, dependencies),
		logger: logger,
		mux:    http.NewServeMux(),
	}
	server.mux.HandleFunc("POST "+intakePath, server.handleIntake)
	server.mux.HandleFunc("POST "+statusPath, server.handleStatus)
	server.mux.HandleFunc("GET /healthz", server.handleHealth)
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	startedAt := time.Now()
	requestID := request.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", server.requestSequence.Add(1))
	}
	trackedWriter := &statusWriter{ResponseWriter: writer}
	server.mux.ServeHTTP(trackedWriter, request)
	server.logger.Info("request completed",
		"request_id", requestID,
		"method", request.Method,
		"path", request.URL.Path,
		"status", trackedWriter.status,
		"duration", time.Since(startedAt),
	)
}

func (server *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, "ok\n")
}

func (server *Server) handleIntake(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		server.writeError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "Bearer token is missing or invalid.")
		return
	}
	if request.Header.Get("Accept") != "application/xml" {
		server.writeError(writer, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Accept must be application/xml.")
		return
	}

	payload, err := readMultipartXML(request)
	if err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			server.writeError(writer, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", err.Error())
		} else {
			server.writeError(writer, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		}
		return
	}
	input, err := decodeTransmission(payload)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	receiptID, fault, err := server.store.record(input)
	if errors.Is(err, errDuplicateUTID) {
		server.writeError(writer, http.StatusConflict, "DUPLICATE_UTID", "The UTID is already recorded; retrieve it using the status operation.")
		return
	}
	if errors.Is(err, errDuplicateRecord) {
		server.writeError(writer, http.StatusConflict, "DUPLICATE_RECORD", "An original record is already recorded.")
		return
	}
	if err != nil {
		server.writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "IRIS is temporarily unavailable.")
		return
	}
	if fault == failBeforeRecord || fault == failAfterRecord {
		server.logger.Info("injected intake failure",
			"firm_id", input.firmID,
			"client_id", input.clientID,
			"utid_hash", hashForLog(input.utid),
			"recorded", fault == failAfterRecord,
		)
		server.writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "IRIS is temporarily unavailable.")
		return
	}

	server.logger.Info("transmission recorded",
		"firm_id", input.firmID,
		"client_id", input.clientID,
		"utid_hash", hashForLog(input.utid),
		"receipt_id", receiptID,
		"record_count", len(input.records),
	)
	server.writeXML(writer, http.StatusOK, receiptIDXML{Value: receiptID})
}

func (server *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		server.writeError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "Bearer token is missing or invalid.")
		return
	}
	if request.Header.Get("Accept") != "application/xml" || mediaType(request.Header.Get("Content-Type")) != "application/xml" {
		server.writeError(writer, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type and Accept must be application/xml.")
		return
	}

	payload, err := io.ReadAll(request.Body)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Unable to read XML request.")
		return
	}
	search, err := decodeStatusRequest(payload)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	var recorded *submission
	var found bool
	switch search.SearchParameterType {
	case "RECEIPTID":
		recorded, found = server.store.findByReceiptID(search.TransmitterControlCD, search.SearchParameterText)
	case "UTID":
		recorded, found = server.store.findByUTID(search.TransmitterControlCD, search.SearchParameterText)
	default:
		server.writeError(writer, http.StatusBadRequest, "INVALID_REQUEST", "SearchParameterTypeCd must be RECEIPTID or UTID.")
		return
	}
	if !found {
		server.writeError(writer, http.StatusNotFound, "NOT_FOUND", "The ReceiptId or UTID was not found.")
		return
	}

	response := statusResponseXML{
		ReceiptID:            recorded.receiptID,
		UniqueTransmissionID: recorded.utid,
		TransmissionStatus:   "Processing",
	}
	if !recorded.neverAcknowledges && !server.store.now().Before(recorded.acknowledgmentAvailableAt) {
		response.RecordResults = make([]recordResultXML, 0, len(recorded.results))
		accepted := 0
		for _, result := range recorded.results {
			response.RecordResults = append(response.RecordResults, recordResultXML{
				RecordID:    result.recordID,
				Status:      result.status,
				IRSRecordID: result.irsRecordID,
				ErrorReason: result.errorReason,
			})
			if result.status == "Accepted" {
				accepted++
			}
		}
		switch {
		case accepted == len(recorded.results):
			response.TransmissionStatus = "Accepted"
		case accepted == 0:
			response.TransmissionStatus = "Rejected"
		default:
			response.TransmissionStatus = "PartiallyAccepted"
		}
	}
	server.writeXML(writer, http.StatusOK, response)
}

func (server *Server) authorized(request *http.Request) bool {
	return request.Header.Get("Authorization") == "Bearer "+server.config.BearerToken
}

func readMultipartXML(request *http.Request) ([]byte, error) {
	if mediaType(request.Header.Get("Content-Type")) != "multipart/form-data" {
		return nil, fmt.Errorf("%w: Content-Type must be multipart/form-data", errUnsupportedMediaType)
	}
	reader, err := request.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("invalid multipart request")
	}
	part, err := reader.NextPart()
	if err != nil {
		return nil, fmt.Errorf("multipart request must contain one file part")
	}
	defer part.Close()
	if part.FormName() != "file" || part.FileName() == "" {
		return nil, fmt.Errorf("multipart part must be a file named file")
	}
	partType := mediaType(part.Header.Get("Content-Type"))
	if partType != "text/xml" && partType != "application/xml" {
		return nil, fmt.Errorf("%w: file media type must be text/xml or application/xml", errUnsupportedMediaType)
	}
	payload, err := io.ReadAll(part)
	if err != nil {
		return nil, fmt.Errorf("unable to read XML file")
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("XML file must not be empty")
	}
	if extra, nextErr := reader.NextPart(); nextErr != io.EOF {
		if nextErr == nil {
			_ = extra.Close()
		}
		return nil, fmt.Errorf("multipart request must contain exactly one file part")
	}
	return payload, nil
}

func decodeTransmission(payload []byte) (transmission, error) {
	if err := validateXMLElements(payload, "IRTransmission", intakeElements); err != nil {
		return transmission{}, err
	}
	var document transmissionXML
	if err := xml.Unmarshal(payload, &document); err != nil {
		return transmission{}, fmt.Errorf("invalid transmission XML: %w", err)
	}
	if len(document.Manifests) != 1 || len(document.Submissions) != 1 {
		return transmission{}, fmt.Errorf("transmission must contain exactly one manifest and one submission")
	}
	manifest := document.Manifests[0]
	submissionGroup := document.Submissions[0]
	if len(submissionGroup.Headers) != 1 {
		return transmission{}, fmt.Errorf("submission must contain exactly one header")
	}
	header := submissionGroup.Headers[0]
	if manifest.UniqueTransmissionID == "" || manifest.TransmitterControlCD == "" || header.SubmissionID == "" || header.ClientID == "" {
		return transmission{}, fmt.Errorf("UTID, transmitter, submission, and client are required")
	}
	if manifest.TransmissionTypeCD != "O" || manifest.TaxYear != "2025" || header.FormTypeCD != "1099-NEC" {
		return transmission{}, fmt.Errorf("only original tax-year 2025 Form 1099-NEC transmissions are accepted")
	}
	utidPattern := regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}:IRIS:` + regexp.QuoteMeta(manifest.TransmitterControlCD) + `::A$`)
	if !utidPattern.MatchString(manifest.UniqueTransmissionID) {
		return transmission{}, fmt.Errorf("UTID must match <UUID>:IRIS:<TransmitterControlCd>::A")
	}
	if len(submissionGroup.Records) < 1 || len(submissionGroup.Records) > 100 || header.ReportedRecipientCnt != len(submissionGroup.Records) {
		return transmission{}, fmt.Errorf("submission must contain 1-100 records and the header count must match")
	}

	input := transmission{
		firmID:       manifest.TransmitterControlCD,
		clientID:     header.ClientID,
		taxYear:      manifest.TaxYear,
		utid:         manifest.UniqueTransmissionID,
		submissionID: header.SubmissionID,
		records:      make([]filingRecord, 0, len(submissionGroup.Records)),
	}
	seenRecordIDs := make(map[string]struct{}, len(submissionGroup.Records))
	for _, record := range submissionGroup.Records {
		if record.RecordID == "" || record.NonemployeeCompensationAmount == "" || record.FederalIncomeTaxWithheldAmount == "" {
			return transmission{}, fmt.Errorf("record ID and amount fields are required")
		}
		if _, exists := seenRecordIDs[record.RecordID]; exists {
			return transmission{}, fmt.Errorf("record IDs must be unique within a submission")
		}
		seenRecordIDs[record.RecordID] = struct{}{}
		amountCents, err := parseCents(record.NonemployeeCompensationAmount)
		if err != nil {
			return transmission{}, fmt.Errorf("invalid nonemployee compensation for record %q", record.RecordID)
		}
		withholdingCents, err := parseCents(record.FederalIncomeTaxWithheldAmount)
		if err != nil {
			return transmission{}, fmt.Errorf("invalid federal withholding for record %q", record.RecordID)
		}
		input.records = append(input.records, filingRecord{
			recordID:         record.RecordID,
			recipientName:    record.RecipientName,
			recipientTIN:     record.RecipientTIN,
			amountCents:      amountCents,
			withholdingCents: withholdingCents,
		})
	}
	return input, nil
}

func decodeStatusRequest(payload []byte) (statusRequestXML, error) {
	if err := validateXMLElements(payload, "TransStatusOrAckRequest", statusElements); err != nil {
		return statusRequestXML{}, err
	}
	var request statusRequestXML
	if err := xml.Unmarshal(payload, &request); err != nil {
		return statusRequestXML{}, fmt.Errorf("invalid status XML: %w", err)
	}
	if request.TransmitterControlCD == "" || request.SearchParameterText == "" {
		return statusRequestXML{}, fmt.Errorf("transmitter and search parameter are required")
	}
	return request, nil
}

func validateXMLElements(payload []byte, rootName string, allowed map[string]struct{}) error {
	decoder := xml.NewDecoder(strings.NewReader(string(payload)))
	depth := 0
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != "" || len(value.Attr) != 0 {
				return fmt.Errorf("XML namespaces and attributes are not supported")
			}
			if _, ok := allowed[value.Name.Local]; !ok {
				return fmt.Errorf("unknown XML element %q", value.Name.Local)
			}
			if depth == 0 {
				if rootSeen || value.Name.Local != rootName {
					return fmt.Errorf("root element must be %s", rootName)
				}
				rootSeen = true
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(value)) != "" {
				return fmt.Errorf("unexpected data outside root element")
			}
		}
	}
	if !rootSeen || depth != 0 {
		return fmt.Errorf("XML document is empty or incomplete")
	}
	return nil
}

func parseCents(value string) (int64, error) {
	if !moneyPattern.MatchString(value) {
		return 0, fmt.Errorf("money must have exactly two decimal places")
	}
	compact := strings.Replace(value, ".", "", 1)
	cents, err := strconv.ParseInt(compact, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money is outside the supported range")
	}
	return cents, nil
}

func mediaType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return parsed
}

func hashForLog(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func (server *Server) writeError(writer http.ResponseWriter, status int, code, message string) {
	server.writeXML(writer, status, errorResponseXML{Code: code, Message: message})
}

func (server *Server) writeXML(writer http.ResponseWriter, status int, value any) {
	payload, err := xml.Marshal(value)
	if err != nil {
		http.Error(writer, "unable to encode response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}
