// Package protocol defines the trusted boundary between an Orpheus review
// sandbox and the connector.
package protocol

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	SchemaVersion = 1
	Encoding      = "zlib+base64"
	StageReady    = "ready"
)

type Envelope struct {
	SchemaVersion     int    `json:"schema_version"`
	Encoding          string `json:"encoding"`
	PayloadSHA256     string `json:"payload_sha256"`
	UncompressedBytes int    `json:"uncompressed_bytes"`
	Payload           string `json:"payload"`
}

type Bundle struct {
	SchemaVersion   int              `json:"schema_version"`
	Stage           string           `json:"stage"`
	Workflow        WorkflowIdentity `json:"workflow"`
	Identity        ReviewIdentity   `json:"identity"`
	Review          ReviewIdentityV1 `json:"review"`
	Counts          Counts           `json:"counts"`
	Confirmed       []Finding        `json:"confirmed"`
	Recommendations []Recommendation `json:"recommendations"`
	Resolutions     []Resolution     `json:"resolutions"`
}

type WorkflowIdentity struct {
	ID           string `json:"id"`
	Revision     string `json:"revision"`
	HelperSHA256 string `json:"helper_sha256"`
}

type ReviewIdentity struct {
	GitLabHost      string `json:"gitlab_host"`
	ProjectID       int64  `json:"project_id"`
	MergeRequestIID int64  `json:"mr_iid"`
	ReviewerUserID  int64  `json:"reviewer_user_id"`
}

type ReviewIdentityV1 struct {
	DiffFingerprint   string `json:"diff_fingerprint"`
	ReviewFingerprint string `json:"review_fingerprint"`
}

type Counts struct {
	Confirmed       int `json:"confirmed"`
	Rejected        int `json:"rejected"`
	Recommendations int `json:"recommendations"`
	Resolutions     int `json:"resolutions"`
	Pending         int `json:"pending"`
}

type Finding struct {
	ID       string           `json:"id"`
	Path     string           `json:"path"`
	Line     int              `json:"line"`
	Severity string           `json:"severity"`
	Title    string           `json:"title"`
	Source   string           `json:"source"`
	Body     string           `json:"body"`
	Previous *PreviousFinding `json:"previous,omitempty"`
}

// PreviousFinding links a recurring finding to the exact Orpheus note which
// published it in an earlier review. Publication verifies the author, marker,
// and discussion state against a fresh GitLab snapshot before trusting it.
type PreviousFinding struct {
	DiscussionID      string `json:"discussion_id"`
	NoteID            int64  `json:"note_id"`
	Marker            string `json:"marker"`
	RecurrenceComment string `json:"recurrence_comment"`
}

type Recommendation struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type Resolution struct {
	DiscussionID string `json:"discussion_id"`
	NoteID       int64  `json:"note_id"`
	Marker       string `json:"marker"`
	Body         string `json:"body"`
}

type Expected struct {
	Workflow WorkflowIdentity
	Identity ReviewIdentity
	Review   ReviewIdentityV1
}

type Limits struct {
	MaxEnvelopeBytes int
	MaxPayloadBytes  int
	MaxItems         int
	MaxFieldBytes    int
	MaxBodyBytes     int
}

func DefaultLimits() Limits {
	return Limits{
		MaxEnvelopeBytes: 480 << 10,
		MaxPayloadBytes:  4 << 20,
		MaxItems:         1_000,
		MaxFieldBytes:    4 << 10,
		MaxBodyBytes:     256 << 10,
	}
}

type ValidationError struct {
	Code string
	Err  error
}

func (e *ValidationError) Error() string {
	if e.Err == nil {
		return "invalid review bundle: " + e.Code
	}

	return fmt.Sprintf("invalid review bundle: %s: %v", e.Code, e.Err)
}

func (e *ValidationError) Unwrap() error { return e.Err }

func ErrorCode(err error) string {
	var validationError *ValidationError
	if errors.As(err, &validationError) {
		return validationError.Code
	}

	return ""
}

func Decode(output []byte, limits Limits, expected Expected) (Bundle, error) {
	if err := validateLimits(limits); err != nil {
		return Bundle{}, err
	}
	if len(output) > limits.MaxEnvelopeBytes {
		return Bundle{}, invalid("envelope_too_large", nil)
	}

	var envelope Envelope
	if err := decodeSingleJSON(output, &envelope); err != nil {
		return Bundle{}, invalid("invalid_envelope_json", err)
	}
	if envelope.SchemaVersion != SchemaVersion {
		return Bundle{}, invalid("unsupported_envelope_schema", nil)
	}
	if envelope.Encoding != Encoding {
		return Bundle{}, invalid("unsupported_envelope_encoding", nil)
	}
	if envelope.UncompressedBytes < 0 || envelope.UncompressedBytes > limits.MaxPayloadBytes {
		return Bundle{}, invalid("declared_payload_size_out_of_bounds", nil)
	}

	compressed, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return Bundle{}, invalid("invalid_payload_base64", err)
	}
	payload, err := decompressBounded(compressed, limits.MaxPayloadBytes)
	if err != nil {
		return Bundle{}, err
	}
	if len(payload) != envelope.UncompressedBytes {
		return Bundle{}, invalid("payload_size_mismatch", nil)
	}
	if !validDigest(envelope.PayloadSHA256, payload) {
		return Bundle{}, invalid("payload_digest_mismatch", nil)
	}

	var bundle Bundle
	if err := decodeSingleJSON(payload, &bundle); err != nil {
		return Bundle{}, invalid("invalid_payload_json", err)
	}
	if err := Validate(bundle, limits, expected); err != nil {
		return Bundle{}, err
	}

	return bundle, nil
}

func Encode(bundle Bundle, limits Limits) ([]byte, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encode review bundle: %w", err)
	}
	if len(payload) > limits.MaxPayloadBytes {
		return nil, invalid("payload_too_large", nil)
	}

	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		return nil, fmt.Errorf("compress review bundle: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("compress review bundle: %w", err)
	}
	digest := sha256.Sum256(payload)
	envelope := Envelope{
		SchemaVersion:     SchemaVersion,
		Encoding:          Encoding,
		PayloadSHA256:     "sha256:" + hex.EncodeToString(digest[:]),
		UncompressedBytes: len(payload),
		Payload:           base64.StdEncoding.EncodeToString(compressed.Bytes()),
	}
	output, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode review envelope: %w", err)
	}
	if len(output) > limits.MaxEnvelopeBytes {
		return nil, invalid("envelope_too_large", nil)
	}

	return output, nil
}

func Validate(bundle Bundle, limits Limits, expected Expected) error {
	if err := validateLimits(limits); err != nil {
		return err
	}
	if bundle.SchemaVersion != SchemaVersion {
		return invalid("unsupported_bundle_schema", nil)
	}
	if bundle.Stage != StageReady {
		return invalid("bundle_not_ready", nil)
	}
	if bundle.Counts.Pending != 0 {
		return invalid("pending_findings", nil)
	}
	if bundle.Counts.Confirmed != len(bundle.Confirmed) ||
		bundle.Counts.Recommendations != len(bundle.Recommendations) ||
		bundle.Counts.Resolutions != len(bundle.Resolutions) {
		return invalid("count_mismatch", nil)
	}
	if bundle.Counts.Rejected < 0 {
		return invalid("count_mismatch", nil)
	}
	items := len(bundle.Confirmed) + bundle.Counts.Rejected + len(bundle.Recommendations) + len(bundle.Resolutions)
	if items > limits.MaxItems {
		return invalid("too_many_items", nil)
	}
	if bundle.Workflow != expected.Workflow {
		return invalid("workflow_identity_mismatch", nil)
	}
	if bundle.Identity != expected.Identity {
		return invalid("review_identity_mismatch", nil)
	}
	if bundle.Review != expected.Review {
		return invalid("review_fingerprint_mismatch", nil)
	}
	if !slices.IsSortedFunc(bundle.Confirmed, compareFindings) {
		return invalid("findings_not_sorted", nil)
	}
	if !slices.IsSortedFunc(bundle.Recommendations, func(a, b Recommendation) int { return strings.Compare(a.Name, b.Name) }) {
		return invalid("recommendations_not_sorted", nil)
	}
	if !slices.IsSortedFunc(bundle.Resolutions, compareResolutions) {
		return invalid("resolutions_not_sorted", nil)
	}

	seenIDs := make(map[string]struct{}, len(bundle.Confirmed))
	for _, finding := range bundle.Confirmed {
		if err := validateFinding(finding, limits, seenIDs); err != nil {
			return err
		}
	}
	for _, recommendation := range bundle.Recommendations {
		if !validText(recommendation.Name, limits.MaxFieldBytes) || !validText(recommendation.Body, limits.MaxBodyBytes) {
			return invalid("invalid_recommendation", nil)
		}
	}
	for _, resolution := range bundle.Resolutions {
		if !validText(resolution.DiscussionID, limits.MaxFieldBytes) || resolution.NoteID <= 0 ||
			!validText(resolution.Marker, limits.MaxFieldBytes) || !validText(resolution.Body, limits.MaxBodyBytes) {
			return invalid("invalid_resolution", nil)
		}
	}

	return nil
}

func validateFinding(finding Finding, limits Limits, seen map[string]struct{}) error {
	if !validText(finding.ID, limits.MaxFieldBytes) {
		return invalid("invalid_finding_id", nil)
	}
	if _, exists := seen[finding.ID]; exists {
		return invalid("duplicate_finding_id", nil)
	}
	seen[finding.ID] = struct{}{}
	if !validRepositoryPath(finding.Path, limits.MaxFieldBytes) {
		return invalid("invalid_finding_path", nil)
	}
	if finding.Line <= 0 {
		return invalid("invalid_finding_line", nil)
	}
	if finding.Severity != "info" && finding.Severity != "warning" && finding.Severity != "error" {
		return invalid("invalid_finding_severity", nil)
	}
	if !validText(finding.Title, limits.MaxFieldBytes) || !validText(finding.Source, limits.MaxFieldBytes) ||
		!validText(finding.Body, limits.MaxBodyBytes) {
		return invalid("invalid_finding_content", nil)
	}
	if finding.Previous != nil {
		previous := finding.Previous
		if !validText(previous.DiscussionID, limits.MaxFieldBytes) || previous.NoteID <= 0 ||
			!validText(previous.Marker, limits.MaxFieldBytes) ||
			!validText(previous.RecurrenceComment, limits.MaxBodyBytes) {
			return invalid("invalid_previous_finding", nil)
		}
	}

	return nil
}

func validRepositoryPath(value string, limit int) bool {
	return validText(value, limit) && !path.IsAbs(value) && path.Clean(value) == value && value != "." &&
		value != ".." && !strings.HasPrefix(value, "../") && !strings.Contains(value, "\\")
}

func validText(value string, limit int) bool {
	return value != "" && strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00')
}

func compareFindings(a, b Finding) int { return strings.Compare(a.ID, b.ID) }

func compareResolutions(a, b Resolution) int {
	if compared := strings.Compare(a.DiscussionID, b.DiscussionID); compared != 0 {
		return compared
	}
	if a.NoteID < b.NoteID {
		return -1
	}
	if a.NoteID > b.NoteID {
		return 1
	}
	return 0
}

func decodeSingleJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}

	return nil
}

func decompressBounded(compressed []byte, limit int) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, invalid("invalid_payload_compression", err)
	}
	defer func() { _ = reader.Close() }()

	payload, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, invalid("invalid_payload_compression", err)
	}
	if len(payload) > limit {
		return nil, invalid("payload_too_large", nil)
	}

	return payload, nil
}

func validDigest(value string, payload []byte) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return false
	}
	actual := sha256.Sum256(payload)
	return bytes.Equal(expected, actual[:])
}

func validateLimits(limits Limits) error {
	if limits.MaxEnvelopeBytes <= 0 || limits.MaxPayloadBytes <= 0 || limits.MaxItems <= 0 ||
		limits.MaxFieldBytes <= 0 || limits.MaxBodyBytes <= 0 {
		return errors.New("review protocol limits must be positive")
	}

	return nil
}

func invalid(code string, err error) error {
	return &ValidationError{Code: code, Err: err}
}
