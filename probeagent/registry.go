package probeagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	RegistryVersion             = 1
	DefaultEnrollmentTTL        = 15 * time.Minute
	DefaultHeartbeatIntervalSec = 30
	DefaultHeartbeatMaxSkew     = 2 * time.Minute
)

var (
	ErrDisabled              = errors.New("remote diagnostics is disabled")
	ErrInvalidAgent          = errors.New("invalid probe agent")
	ErrAgentNotFound         = errors.New("probe agent not found")
	ErrAgentRevoked          = errors.New("probe agent is revoked")
	ErrAgentNotRevoked       = errors.New("probe agent must be revoked before deletion")
	ErrSourceIPMismatch      = errors.New("probe agent source IP mismatch")
	ErrEnrollmentExpired     = errors.New("probe agent enrollment expired")
	ErrEnrollmentUsed        = errors.New("probe agent enrollment already used")
	ErrInvalidEnrollment     = errors.New("invalid probe agent enrollment")
	ErrAgentNotEnrolled      = errors.New("probe agent is not enrolled")
	ErrInvalidControlRequest = errors.New("invalid probe agent control request")
	ErrControlReplay         = errors.New("replayed probe agent control request")
)

type AgentRecord struct {
	AgentID              string    `json:"agentId"`
	DisplayName          string    `json:"displayName"`
	ExpectedSourceIP     string    `json:"expectedSourceIp"`
	ControllerIP         string    `json:"controllerIp"`
	ControllerURL        string    `json:"controllerUrl"`
	Region               string    `json:"region,omitempty"`
	Provider             string    `json:"provider,omitempty"`
	NetworkGroup         string    `json:"networkGroup,omitempty"`
	Enabled              bool      `json:"enabled"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
	EnrollmentExpiresAt  time.Time `json:"enrollmentExpiresAt,omitempty"`
	EnrollmentUsedAt     time.Time `json:"enrollmentUsedAt,omitempty"`
	EnrollmentTokenHash  string    `json:"enrollmentTokenHash,omitempty"`
	IdentityPublicKey    string    `json:"identityPublicKey,omitempty"`
	ObservationPublicKey string    `json:"observationPublicKey,omitempty"`
	EnrolledAt           time.Time `json:"enrolledAt,omitempty"`
	LastSeenAt           time.Time `json:"lastSeenAt,omitempty"`
	LastSequence         uint64    `json:"lastSequence,omitempty"`
	AgentVersion         string    `json:"agentVersion,omitempty"`
	Capabilities         []string  `json:"capabilities,omitempty"`
	Health               string    `json:"health,omitempty"`
	RevokedAt            time.Time `json:"revokedAt,omitempty"`
}

type RegistryFile struct {
	Version   int                    `json:"version"`
	UpdatedAt time.Time              `json:"updatedAt"`
	Agents    map[string]AgentRecord `json:"agents"`
}

type AgentSnapshot struct {
	AgentID             string    `json:"agentId"`
	DisplayName         string    `json:"displayName"`
	ExpectedSourceIP    string    `json:"expectedSourceIp"`
	ControllerIP        string    `json:"controllerIp"`
	ControllerURL       string    `json:"controllerUrl"`
	Region              string    `json:"region,omitempty"`
	Provider            string    `json:"provider,omitempty"`
	NetworkGroup        string    `json:"networkGroup,omitempty"`
	Enabled             bool      `json:"enabled"`
	Connected           bool      `json:"connected"`
	CreatedAt           time.Time `json:"createdAt"`
	EnrollmentExpiresAt time.Time `json:"enrollmentExpiresAt,omitempty"`
	EnrollmentUsedAt    time.Time `json:"enrollmentUsedAt,omitempty"`
	EnrolledAt          time.Time `json:"enrolledAt,omitempty"`
	LastSeenAt          time.Time `json:"lastSeenAt,omitempty"`
	AgentVersion        string    `json:"agentVersion,omitempty"`
	Capabilities        []string  `json:"capabilities,omitempty"`
	Health              string    `json:"health,omitempty"`
	// encoding/json never omits a struct, so RevokedAt is always encoded and a
	// zero value still reaches clients as "0001-01-01T00:00:00Z". Revoked is the
	// field consumers must branch on.
	RevokedAt          time.Time `json:"revokedAt,omitempty"`
	Revoked            bool      `json:"revoked"`
	IdentityConfigured bool      `json:"identityConfigured"`
	ObservationKeySet  bool      `json:"observationKeySet"`
}

type CreateAgentRequest struct {
	DisplayName      string `json:"displayName"`
	ExpectedSourceIP string `json:"expectedSourceIp"`
	ControllerIP     string `json:"controllerIp"`
	ControllerURL    string `json:"controllerUrl"`
	Region           string `json:"region,omitempty"`
	Provider         string `json:"provider,omitempty"`
	NetworkGroup     string `json:"networkGroup,omitempty"`
}

type CreationResult struct {
	Agent           AgentSnapshot `json:"agent"`
	EnrollmentToken string        `json:"enrollmentToken"`
	Compose         string        `json:"compose"`
}

type RegistryConfig struct {
	Path                 string
	Enabled              bool
	AgentImage           string
	EnrollmentTTL        time.Duration
	HeartbeatMaxSkew     time.Duration
	HeartbeatIntervalSec int
	Now                  func() time.Time
	Random               io.Reader
}

type Registry struct {
	mu     sync.RWMutex
	config RegistryConfig
	state  RegistryFile
}

func NewRegistry(config RegistryConfig) (*Registry, error) {
	config.Path = strings.TrimSpace(config.Path)
	if config.Path == "" {
		return nil, fmt.Errorf("%w: registry path is required", ErrInvalidAgent)
	}
	if config.EnrollmentTTL == 0 {
		config.EnrollmentTTL = DefaultEnrollmentTTL
	}
	if config.HeartbeatMaxSkew == 0 {
		config.HeartbeatMaxSkew = DefaultHeartbeatMaxSkew
	}
	if config.HeartbeatIntervalSec == 0 {
		config.HeartbeatIntervalSec = DefaultHeartbeatIntervalSec
	}
	if config.EnrollmentTTL < time.Minute || config.HeartbeatMaxSkew < time.Second || config.HeartbeatIntervalSec < 5 {
		return nil, fmt.Errorf("%w: registry time limits are invalid", ErrInvalidAgent)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if strings.TrimSpace(config.AgentImage) == "" {
		config.AgentImage = "ghcr.io/invisibleproxy/xray-checker-probe-agent:main"
	}
	return &Registry{
		config: config,
		state:  RegistryFile{Version: RegistryVersion, Agents: make(map[string]AgentRecord)},
	}, nil
}

func (r *Registry) Enabled() bool {
	return r != nil && r.config.Enabled
}

func (r *Registry) Load() error {
	if r == nil {
		return fmt.Errorf("probe agent registry is nil")
	}
	data, err := os.ReadFile(r.config.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read probe agent registry: %w", err)
	}
	state, err := DecodeRegistry(data)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
	return nil
}

func DecodeRegistry(data []byte) (RegistryFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state RegistryFile
	if err := decoder.Decode(&state); err != nil {
		return RegistryFile{}, fmt.Errorf("decode probe agent registry: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RegistryFile{}, fmt.Errorf("decode probe agent registry: trailing JSON data")
	}
	if state.Version == 0 {
		state.Version = RegistryVersion
	}
	if state.Version != RegistryVersion {
		return RegistryFile{}, fmt.Errorf("unsupported probe agent registry version %d", state.Version)
	}
	if state.Agents == nil {
		state.Agents = make(map[string]AgentRecord)
	}
	for id, record := range state.Agents {
		record.AgentID = strings.TrimSpace(record.AgentID)
		if record.AgentID == "" {
			record.AgentID = id
		}
		record.Capabilities = normalizeCapabilities(record.Capabilities)
		if err := validateRecord(record); err != nil {
			return RegistryFile{}, fmt.Errorf("probe agent %q: %w", id, err)
		}
		if id != record.AgentID {
			return RegistryFile{}, fmt.Errorf("probe agent key %q does not match agentId %q", id, record.AgentID)
		}
		state.Agents[id] = record
	}
	return state, nil
}

func (r *Registry) Snapshot() []AgentSnapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]AgentSnapshot, 0, len(r.state.Agents))
	for _, record := range r.state.Agents {
		result = append(result, r.snapshot(record))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (r *Registry) Agent(agentID string) (AgentSnapshot, bool) {
	if r == nil {
		return AgentSnapshot{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.state.Agents[strings.TrimSpace(agentID)]
	if !ok {
		return AgentSnapshot{}, false
	}
	return r.snapshot(record), true
}

// ObservationPublicKey exposes only the enrolled public verification key.
// Revoked or disabled identities fail closed and cannot complete jobs that
// were already dispatched.
func (r *Registry) ObservationPublicKey(agentID string) (ed25519.PublicKey, bool) {
	if r == nil || !r.Enabled() {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.state.Agents[strings.TrimSpace(agentID)]
	if !ok || !record.Enabled || !record.RevokedAt.IsZero() || record.EnrolledAt.IsZero() {
		return nil, false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(record.ObservationPublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(append([]byte(nil), decoded...)), true
}

func (r *Registry) Create(request CreateAgentRequest) (CreationResult, error) {
	if !r.Enabled() {
		return CreationResult{}, ErrDisabled
	}
	request = normalizeCreateRequest(request)
	if err := validateCreateRequest(request); err != nil {
		return CreationResult{}, err
	}
	agentID, err := r.randomToken("agent", 16)
	if err != nil {
		return CreationResult{}, err
	}
	token, err := r.randomToken("enroll", 32)
	if err != nil {
		return CreationResult{}, err
	}
	now := r.now()
	record := AgentRecord{
		AgentID:             agentID,
		DisplayName:         request.DisplayName,
		ExpectedSourceIP:    request.ExpectedSourceIP,
		ControllerIP:        request.ControllerIP,
		ControllerURL:       request.ControllerURL,
		Region:              request.Region,
		Provider:            request.Provider,
		NetworkGroup:        request.NetworkGroup,
		Enabled:             true,
		CreatedAt:           now,
		UpdatedAt:           now,
		EnrollmentExpiresAt: now.Add(r.config.EnrollmentTTL),
		EnrollmentTokenHash: tokenHash(token),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.state.Agents[agentID]; exists {
		return CreationResult{}, fmt.Errorf("%w: generated agent ID collision", ErrInvalidAgent)
	}
	previousUpdatedAt := r.state.UpdatedAt
	r.state.Agents[agentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		delete(r.state.Agents, agentID)
		r.state.UpdatedAt = previousUpdatedAt
		return CreationResult{}, err
	}
	return r.creationResult(record, token), nil
}

func (r *Registry) Reissue(agentID string) (CreationResult, error) {
	if !r.Enabled() {
		return CreationResult{}, ErrDisabled
	}
	token, err := r.randomToken("enroll", 32)
	if err != nil {
		return CreationResult{}, err
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.state.Agents[strings.TrimSpace(agentID)]
	if !ok {
		return CreationResult{}, ErrAgentNotFound
	}
	previous := record
	previousUpdatedAt := r.state.UpdatedAt
	record.Enabled = true
	record.RevokedAt = time.Time{}
	record.EnrollmentExpiresAt = now.Add(r.config.EnrollmentTTL)
	record.EnrollmentUsedAt = time.Time{}
	record.EnrollmentTokenHash = tokenHash(token)
	record.IdentityPublicKey = ""
	record.ObservationPublicKey = ""
	record.EnrolledAt = time.Time{}
	record.LastSeenAt = time.Time{}
	record.LastSequence = 0
	record.AgentVersion = ""
	record.Capabilities = nil
	record.Health = ""
	record.UpdatedAt = now
	r.state.Agents[record.AgentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[record.AgentID] = previous
		r.state.UpdatedAt = previousUpdatedAt
		return CreationResult{}, err
	}
	return r.creationResult(record, token), nil
}

func (r *Registry) Revoke(agentID string) (AgentSnapshot, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.state.Agents[strings.TrimSpace(agentID)]
	if !ok {
		return AgentSnapshot{}, ErrAgentNotFound
	}
	previous := record
	previousUpdatedAt := r.state.UpdatedAt
	record.Enabled = false
	record.RevokedAt = now
	record.EnrollmentTokenHash = ""
	record.UpdatedAt = now
	r.state.Agents[record.AgentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[record.AgentID] = previous
		r.state.UpdatedAt = previousUpdatedAt
		return AgentSnapshot{}, err
	}
	return r.snapshot(record), nil
}

func (r *Registry) Delete(agentID string) error {
	if !r.Enabled() {
		return ErrDisabled
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	agentID = strings.TrimSpace(agentID)
	record, ok := r.state.Agents[agentID]
	if !ok {
		return ErrAgentNotFound
	}
	if record.Enabled || record.RevokedAt.IsZero() {
		return ErrAgentNotRevoked
	}
	previousUpdatedAt := r.state.UpdatedAt
	delete(r.state.Agents, agentID)
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[agentID] = record
		r.state.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (r *Registry) Enroll(request EnrollRequest, sourceIP netip.Addr) (EnrollResponse, error) {
	if !r.Enabled() {
		return EnrollResponse{}, ErrDisabled
	}
	if request.ProtocolVersion != ProtocolVersion || len(request.IdentityPublicKey) != ed25519.PublicKeySize || len(request.ObservationPublicKey) != ed25519.PublicKeySize || bytes.Equal(request.IdentityPublicKey, request.ObservationPublicKey) {
		return EnrollResponse{}, ErrInvalidEnrollment
	}
	if !validShortText(request.AgentVersion, 128) {
		return EnrollResponse{}, ErrInvalidEnrollment
	}
	request.Capabilities = normalizeCapabilities(request.Capabilities)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.state.Agents[strings.TrimSpace(request.AgentID)]
	if !ok {
		return EnrollResponse{}, ErrAgentNotFound
	}
	if !record.Enabled {
		return EnrollResponse{}, ErrAgentRevoked
	}
	if !sourceMatches(record.ExpectedSourceIP, sourceIP) {
		return EnrollResponse{}, ErrSourceIPMismatch
	}
	if !record.EnrollmentUsedAt.IsZero() || record.EnrollmentTokenHash == "" {
		return EnrollResponse{}, ErrEnrollmentUsed
	}
	if !record.EnrollmentExpiresAt.After(now) {
		return EnrollResponse{}, ErrEnrollmentExpired
	}
	if !tokenMatches(record.EnrollmentTokenHash, request.EnrollmentToken) {
		return EnrollResponse{}, ErrInvalidEnrollment
	}
	previous := record
	previousUpdatedAt := r.state.UpdatedAt
	record.IdentityPublicKey = base64.RawStdEncoding.EncodeToString(request.IdentityPublicKey)
	record.ObservationPublicKey = base64.RawStdEncoding.EncodeToString(request.ObservationPublicKey)
	record.EnrollmentUsedAt = now
	record.EnrollmentTokenHash = ""
	record.EnrolledAt = now
	record.LastSeenAt = now
	record.AgentVersion = request.AgentVersion
	record.Capabilities = request.Capabilities
	record.Health = "starting"
	record.UpdatedAt = now
	r.state.Agents[record.AgentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[record.AgentID] = previous
		r.state.UpdatedAt = previousUpdatedAt
		return EnrollResponse{}, err
	}
	return EnrollResponse{AgentID: record.AgentID, EnrolledAt: now, HeartbeatInterval: r.config.HeartbeatIntervalSec}, nil
}

func (r *Registry) AcceptHeartbeat(request HeartbeatRequest, sourceIP netip.Addr, timestamp time.Time, sequence uint64, payload, signature []byte) (HeartbeatResponse, error) {
	if !r.Enabled() {
		return HeartbeatResponse{}, ErrDisabled
	}
	if request.ProtocolVersion != ProtocolVersion || request.AgentID == "" || !validShortText(request.AgentVersion, 128) || !validHealth(request.Health) || timestamp.IsZero() || sequence == 0 {
		return HeartbeatResponse{}, ErrInvalidControlRequest
	}
	request.Capabilities = normalizeCapabilities(request.Capabilities)
	now := r.now()
	if timestamp.Before(now.Add(-r.config.HeartbeatMaxSkew)) || timestamp.After(now.Add(r.config.HeartbeatMaxSkew)) {
		return HeartbeatResponse{}, ErrInvalidControlRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.state.Agents[strings.TrimSpace(request.AgentID)]
	if !ok {
		return HeartbeatResponse{}, ErrAgentNotFound
	}
	if !record.Enabled {
		return HeartbeatResponse{}, ErrAgentRevoked
	}
	if !sourceMatches(record.ExpectedSourceIP, sourceIP) {
		return HeartbeatResponse{}, ErrSourceIPMismatch
	}
	if record.IdentityPublicKey == "" || record.EnrolledAt.IsZero() {
		return HeartbeatResponse{}, ErrAgentNotEnrolled
	}
	if sequence <= record.LastSequence {
		return HeartbeatResponse{}, ErrControlReplay
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(record.IdentityPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return HeartbeatResponse{}, ErrInvalidControlRequest
	}
	previous := record
	previousUpdatedAt := r.state.UpdatedAt
	record.LastSeenAt = now
	record.LastSequence = sequence
	record.AgentVersion = request.AgentVersion
	record.Capabilities = request.Capabilities
	record.Health = request.Health
	record.UpdatedAt = now
	r.state.Agents[record.AgentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[record.AgentID] = previous
		r.state.UpdatedAt = previousUpdatedAt
		return HeartbeatResponse{}, err
	}
	return HeartbeatResponse{AgentID: record.AgentID, AcceptedAt: now}, nil
}

// AcceptControlRequest authenticates a non-heartbeat agent request with the
// same exact source-IP, timestamp, identity and persisted replay protections.
// It intentionally updates no health/capability fields.
func (r *Registry) AcceptControlRequest(agentID string, sourceIP netip.Addr, timestamp time.Time, sequence uint64, payload, signature []byte) error {
	if !r.Enabled() {
		return ErrDisabled
	}
	agentID = strings.TrimSpace(agentID)
	if !validIdentifier(agentID, 96) || timestamp.IsZero() || sequence == 0 {
		return ErrInvalidControlRequest
	}
	now := r.now()
	if timestamp.Before(now.Add(-r.config.HeartbeatMaxSkew)) || timestamp.After(now.Add(r.config.HeartbeatMaxSkew)) {
		return ErrInvalidControlRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.state.Agents[agentID]
	if !ok {
		return ErrAgentNotFound
	}
	if !record.Enabled || !record.RevokedAt.IsZero() {
		return ErrAgentRevoked
	}
	if !sourceMatches(record.ExpectedSourceIP, sourceIP) {
		return ErrSourceIPMismatch
	}
	if record.IdentityPublicKey == "" || record.EnrolledAt.IsZero() {
		return ErrAgentNotEnrolled
	}
	if sequence <= record.LastSequence {
		return ErrControlReplay
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(record.IdentityPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return ErrInvalidControlRequest
	}
	previous := record
	previousUpdatedAt := r.state.UpdatedAt
	record.LastSeenAt = now
	record.LastSequence = sequence
	record.UpdatedAt = now
	r.state.Agents[record.AgentID] = record
	r.state.UpdatedAt = now
	if err := r.persistLocked(); err != nil {
		r.state.Agents[record.AgentID] = previous
		r.state.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (r *Registry) creationResult(record AgentRecord, token string) CreationResult {
	return CreationResult{
		Agent:           r.snapshot(record),
		EnrollmentToken: token,
		Compose:         RenderCompose(record, token, r.config.AgentImage),
	}
}

func (r *Registry) now() time.Time {
	return r.config.Now().UTC()
}

func (r *Registry) randomToken(prefix string, bytesCount int) (string, error) {
	buffer := make([]byte, bytesCount)
	if _, err := io.ReadFull(r.config.Random, buffer); err != nil {
		return "", fmt.Errorf("generate probe agent credential: %w", err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (r *Registry) persistLocked() error {
	r.state.Version = RegistryVersion
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode probe agent registry: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(r.config.Path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create probe agent state directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".diagnostic-agents-*.tmp")
	if err != nil {
		return fmt.Errorf("create probe agent registry temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect probe agent registry: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write probe agent registry: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync probe agent registry: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close probe agent registry: %w", err)
	}
	if err := os.Rename(tempPath, r.config.Path); err != nil {
		return fmt.Errorf("publish probe agent registry: %w", err)
	}
	return nil
}

func validateCreateRequest(request CreateAgentRequest) error {
	if !validDisplayName(request.DisplayName) {
		return fmt.Errorf("%w: displayName is required and must be at most 80 characters", ErrInvalidAgent)
	}
	if _, err := parseExactIP(request.ExpectedSourceIP); err != nil {
		return fmt.Errorf("%w: expectedSourceIp must be an exact IPv4 or IPv6 address", ErrInvalidAgent)
	}
	if _, err := parseExactIP(request.ControllerIP); err != nil {
		return fmt.Errorf("%w: controllerIp must be an exact IPv4 or IPv6 address", ErrInvalidAgent)
	}
	parsed, err := url.Parse(request.ControllerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: controllerUrl must be an https URL without credentials, query or fragment", ErrInvalidAgent)
	}
	for name, value := range map[string]string{"region": request.Region, "provider": request.Provider, "networkGroup": request.NetworkGroup} {
		if value != "" && !validShortText(value, 80) {
			return fmt.Errorf("%w: %s contains unsupported characters or is too long", ErrInvalidAgent, name)
		}
	}
	return nil
}

func validateRecord(record AgentRecord) error {
	if !validIdentifier(record.AgentID, 96) || !validDisplayName(record.DisplayName) {
		return ErrInvalidAgent
	}
	request := CreateAgentRequest{
		DisplayName: record.DisplayName, ExpectedSourceIP: record.ExpectedSourceIP,
		ControllerIP: record.ControllerIP, ControllerURL: record.ControllerURL,
		Region: record.Region, Provider: record.Provider, NetworkGroup: record.NetworkGroup,
	}
	if err := validateCreateRequest(request); err != nil {
		return err
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: timestamps are required", ErrInvalidAgent)
	}
	if record.AgentVersion != "" && !validShortText(record.AgentVersion, 128) {
		return fmt.Errorf("%w: invalid agent version", ErrInvalidAgent)
	}
	if record.Health != "" && !validHealth(record.Health) {
		return fmt.Errorf("%w: invalid agent health", ErrInvalidAgent)
	}
	for _, encoded := range []string{record.IdentityPublicKey, record.ObservationPublicKey} {
		if encoded == "" {
			continue
		}
		decoded, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: invalid public key", ErrInvalidAgent)
		}
	}
	if record.EnrollmentTokenHash != "" {
		decoded, err := hex.DecodeString(record.EnrollmentTokenHash)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("%w: invalid enrollment token hash", ErrInvalidAgent)
		}
	}
	identityConfigured := record.IdentityPublicKey != "" || record.ObservationPublicKey != "" || !record.EnrolledAt.IsZero()
	if identityConfigured && (record.IdentityPublicKey == "" || record.ObservationPublicKey == "" || record.EnrolledAt.IsZero() || record.EnrollmentUsedAt.IsZero() || record.EnrollmentTokenHash != "") {
		return fmt.Errorf("%w: inconsistent enrolled identity state", ErrInvalidAgent)
	}
	if record.EnrollmentTokenHash != "" && (record.EnrollmentExpiresAt.IsZero() || !record.EnrollmentUsedAt.IsZero()) {
		return fmt.Errorf("%w: inconsistent enrollment token state", ErrInvalidAgent)
	}
	return nil
}

func normalizeCreateRequest(request CreateAgentRequest) CreateAgentRequest {
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.ExpectedSourceIP = normalizeIP(request.ExpectedSourceIP)
	request.ControllerIP = normalizeIP(request.ControllerIP)
	request.ControllerURL = strings.TrimRight(strings.TrimSpace(request.ControllerURL), "/")
	request.Region = strings.TrimSpace(request.Region)
	request.Provider = strings.TrimSpace(request.Provider)
	request.NetworkGroup = strings.TrimSpace(request.NetworkGroup)
	return request
}

func (r *Registry) snapshot(record AgentRecord) AgentSnapshot {
	connectedWindow := time.Duration(r.config.HeartbeatIntervalSec*3) * time.Second
	if connectedWindow < 90*time.Second {
		connectedWindow = 90 * time.Second
	}
	connected := record.Enabled && record.IdentityPublicKey != "" && record.LastSequence > 0 && record.LastSeenAt.After(r.now().Add(-connectedWindow))
	return AgentSnapshot{
		AgentID: record.AgentID, DisplayName: record.DisplayName,
		ExpectedSourceIP: record.ExpectedSourceIP, ControllerIP: record.ControllerIP,
		ControllerURL: record.ControllerURL, Region: record.Region, Provider: record.Provider,
		NetworkGroup: record.NetworkGroup, Enabled: record.Enabled, Connected: connected, CreatedAt: record.CreatedAt,
		EnrollmentExpiresAt: record.EnrollmentExpiresAt, EnrollmentUsedAt: record.EnrollmentUsedAt,
		EnrolledAt: record.EnrolledAt, LastSeenAt: record.LastSeenAt, AgentVersion: record.AgentVersion,
		Capabilities: append([]string(nil), record.Capabilities...), Health: record.Health,
		RevokedAt: record.RevokedAt, Revoked: !record.RevokedAt.IsZero(),
		IdentityConfigured: record.IdentityPublicKey != "",
		ObservationKeySet:  record.ObservationPublicKey != "",
	}
}

func normalizeCapabilities(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if !validIdentifier(value, 64) || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > 16 {
		result = result[:16]
	}
	return result
}

func sourceMatches(expected string, actual netip.Addr) bool {
	expectedIP, err := parseExactIP(expected)
	return err == nil && actual.IsValid() && expectedIP == actual.Unmap()
}

func parseExactIP(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, err
	}
	return address.Unmap(), nil
}

func normalizeIP(value string) string {
	address, err := parseExactIP(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return address.String()
}

func validDisplayName(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= 80 && !strings.ContainsAny(value, "\r\n\x00")
}

func validShortText(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= max && !strings.ContainsAny(value, "\r\n\x00")
}

func validIdentifier(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validHealth(value string) bool {
	switch value {
	case "healthy", "degraded", "unhealthy", "starting":
		return true
	default:
		return false
	}
}

func tokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func tokenMatches(expectedHash, token string) bool {
	want, err := hex.DecodeString(expectedHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want, digest[:]) == 1
}
