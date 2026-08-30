package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
)

func hashSequence(values ...string) [32]byte {
	hash := sha256.New()
	var length [4]byte
	for _, value := range values {
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func filingKey(firmID, clientID string, taxYear int, vendorIdentity string) [32]byte {
	return hashSequence("filing-key-v1", firmID, clientID, fmt.Sprintf("%d", taxYear), "1099-NEC", vendorIdentity, "0")
}

func batchIdentity(firmID, clientID string, taxYear int, filingKeys [][32]byte) ([32]byte, string) {
	values := []string{"batch-ref-v1", firmID, clientID, fmt.Sprintf("%d", taxYear)}
	for _, key := range filingKeys {
		values = append(values, hex.EncodeToString(key[:]))
	}
	digest := hashSequence(values...)
	uuidBytes := digest[:16]
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])
	return digest, uuid + ":IRIS:" + firmID + "::A"
}

type xmlTransmission struct {
	XMLName  xml.Name    `xml:"IRTransmission"`
	Manifest xmlManifest `xml:"IRTransmissionManifest"`
	Group    xmlGroup    `xml:"IRSubmission1Grp"`
}

type xmlManifest struct {
	UTID             string `xml:"UniqueTransmissionId"`
	Transmitter      string `xml:"TransmitterControlCd"`
	TransmissionType string `xml:"TransmissionTypeCd"`
	TaxYear          int    `xml:"TaxYr"`
}

type xmlGroup struct {
	Header  xmlHeader   `xml:"IRSubmission1Header"`
	Details []xmlDetail `xml:"Form1099NECDetail"`
}

type xmlHeader struct {
	SubmissionID string `xml:"SubmissionId"`
	ClientID     string `xml:"ClientId"`
	FormType     string `xml:"FormTypeCd"`
	Count        int    `xml:"ReportedRcpntFormCnt"`
}

type xmlDetail struct {
	RecordID      string `xml:"RecordId"`
	RecipientName string `xml:"RecipientNm"`
	RecipientTIN  string `xml:"RecipientTIN"`
	Compensation  string `xml:"NonemployeeCompensationAmt"`
	Withholding   string `xml:"FederalIncomeTaxWithheldAmt"`
}

func formatCents(cents int64) string {
	sign := ""
	value := cents
	if value < 0 {
		sign = "-"
		value = -(value + 1)
		value++
	}
	return fmt.Sprintf("%s%d.%02d", sign, value/100, value%100)
}

func canonicalXML(firmID, clientID string, taxYear int, utid string, details []xmlDetail, digest [32]byte) ([]byte, error) {
	transmission := xmlTransmission{
		Manifest: xmlManifest{UTID: utid, Transmitter: firmID, TransmissionType: "O", TaxYear: taxYear},
		Group: xmlGroup{
			Header:  xmlHeader{SubmissionID: "SUB-" + strings.ToUpper(hex.EncodeToString(digest[:12])), ClientID: clientID, FormType: "1099-NEC", Count: len(details)},
			Details: details,
		},
	}
	encoded, err := xml.Marshal(transmission)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), encoded...), nil
}
