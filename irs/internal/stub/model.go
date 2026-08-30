package stub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	ackDelayMinimum = 10 * time.Second
	ackDelayRange   = 20 * time.Second
)

type Dependencies struct {
	Now           func() time.Time
	RandomFloat64 func() float64
}

type Store struct {
	mu                       sync.RWMutex
	config                   Config
	now                      func() time.Time
	randomFloat64            func() float64
	submissionsByFirmAndUTID map[firmUTID]*submission
	submissionsByReceiptID   map[string]*submission
	originalRecords          map[originalRecordKey]struct{}
	generatedIDs             map[string]struct{}
}

type firmUTID struct {
	firmID string
	utid   string
}

type originalRecordKey struct {
	firmID   string
	clientID string
	taxYear  string
	recordID string
}

type transmission struct {
	firmID       string
	clientID     string
	taxYear      string
	utid         string
	submissionID string
	records      []filingRecord
}

type filingRecord struct {
	recordID         string
	recipientName    string
	recipientTIN     string
	amountCents      int64
	withholdingCents int64
}

type submission struct {
	receiptID                 string
	utid                      string
	firmID                    string
	clientID                  string
	taxYear                   string
	submissionID              string
	acknowledgmentAvailableAt time.Time
	neverAcknowledges         bool
	results                   []recordResult
}

type recordResult struct {
	recordID    string
	status      string
	irsRecordID string
	errorReason string
}

type intakeFault int

const (
	intakeSucceeded intakeFault = iota
	failBeforeRecord
	failAfterRecord
)

var (
	errDuplicateUTID   = errors.New("duplicate UTID")
	errDuplicateRecord = errors.New("duplicate original record")
)

func newStore(config Config, dependencies Dependencies) *Store {
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.RandomFloat64 == nil {
		dependencies.RandomFloat64 = randomFloat64
	}
	return &Store{
		config:                   config,
		now:                      dependencies.Now,
		randomFloat64:            dependencies.RandomFloat64,
		submissionsByFirmAndUTID: make(map[firmUTID]*submission),
		submissionsByReceiptID:   make(map[string]*submission),
		originalRecords:          make(map[originalRecordKey]struct{}),
		generatedIDs:             make(map[string]struct{}),
	}
}

func randomFloat64() float64 {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	var value uint64
	for _, next := range bytes {
		value = value<<8 | uint64(next)
	}
	return float64(value>>11) / (1 << 53)
}

func (store *Store) record(input transmission) (string, intakeFault, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	utidKey := firmUTID{firmID: input.firmID, utid: input.utid}
	if _, exists := store.submissionsByFirmAndUTID[utidKey]; exists {
		return "", intakeSucceeded, errDuplicateUTID
	}
	for _, record := range input.records {
		key := originalRecordKey{
			firmID:   input.firmID,
			clientID: input.clientID,
			taxYear:  input.taxYear,
			recordID: record.recordID,
		}
		if _, exists := store.originalRecords[key]; exists {
			return "", intakeSucceeded, errDuplicateRecord
		}
	}

	draw := store.randomFloat64() * 100
	fault := classifyIntakeFault(draw, store.config.FailBeforeRecordPercent, store.config.FailAfterRecordPercent)
	if fault == failBeforeRecord {
		return "", failBeforeRecord, nil
	}

	receiptID := store.newOpaqueID("rcpt_")
	results := make([]recordResult, 0, len(input.records))
	for _, record := range input.records {
		results = append(results, store.resultFor(record))
	}
	neverAcknowledges := store.randomFloat64()*100 < store.config.NeverAckPercent
	availableAt := time.Time{}
	if !neverAcknowledges {
		delay := ackDelayMinimum + time.Duration(store.randomFloat64()*float64(ackDelayRange))
		availableAt = store.now().Add(delay)
	}

	recorded := &submission{
		receiptID:                 receiptID,
		utid:                      input.utid,
		firmID:                    input.firmID,
		clientID:                  input.clientID,
		taxYear:                   input.taxYear,
		submissionID:              input.submissionID,
		acknowledgmentAvailableAt: availableAt,
		neverAcknowledges:         neverAcknowledges,
		results:                   results,
	}
	store.submissionsByFirmAndUTID[utidKey] = recorded
	store.submissionsByReceiptID[receiptID] = recorded
	for _, record := range input.records {
		store.originalRecords[originalRecordKey{
			firmID:   input.firmID,
			clientID: input.clientID,
			taxYear:  input.taxYear,
			recordID: record.recordID,
		}] = struct{}{}
	}

	return receiptID, fault, nil
}

func classifyIntakeFault(draw, failBeforePercent, failAfterPercent float64) intakeFault {
	if draw < failBeforePercent {
		return failBeforeRecord
	}
	if draw < failBeforePercent+failAfterPercent {
		return failAfterRecord
	}
	return intakeSucceeded
}

func (store *Store) resultFor(record filingRecord) recordResult {
	result := recordResult{recordID: record.recordID, status: "Rejected"}
	switch {
	case strings.TrimSpace(record.recipientTIN) == "":
		result.errorReason = "TIN_MISSING"
	case !isNineASCIIDigits(record.recipientTIN):
		result.errorReason = "TIN_MALFORMED"
	case strings.HasPrefix(record.recipientTIN, "000"):
		result.errorReason = "TIN_INVALID"
	case record.amountCents <= 0:
		result.errorReason = "AMOUNT_INVALID"
	default:
		result.status = "Accepted"
		result.irsRecordID = store.newOpaqueID("irsrec_")
	}
	return result
}

func isNineASCIIDigits(value string) bool {
	if len(value) != 9 {
		return false
	}
	for index := range value {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func (store *Store) newOpaqueID(prefix string) string {
	for {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		identifier := prefix + hex.EncodeToString(bytes[:])
		if _, exists := store.generatedIDs[identifier]; exists {
			continue
		}
		store.generatedIDs[identifier] = struct{}{}
		return identifier
	}
}

func (store *Store) findByUTID(firmID, utid string) (*submission, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	value, found := store.submissionsByFirmAndUTID[firmUTID{firmID: firmID, utid: utid}]
	return value, found
}

func (store *Store) findByReceiptID(firmID, receiptID string) (*submission, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	value, found := store.submissionsByReceiptID[receiptID]
	if !found || value.firmID != firmID {
		return nil, false
	}
	return value, true
}
