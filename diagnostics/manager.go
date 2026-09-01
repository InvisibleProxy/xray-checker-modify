package diagnostics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSessionTTL            = 24 * time.Hour
	DefaultObservationMaxAge     = 5 * time.Minute
	DefaultClockSkew             = 30 * time.Second
	DefaultMaxSessions           = 100
	DefaultMaxAgentsPerSession   = 8
	MaxObservationSignatureBytes = 512
)

var (
	ErrInvalidSchema        = errors.New("invalid diagnostic schema")
	ErrInvalidRequest       = errors.New("invalid diagnostic request")
	ErrCapacity             = errors.New("diagnostic session capacity reached")
	ErrUnknownSession       = errors.New("unknown diagnostic session")
	ErrUnknownJob           = errors.New("unknown diagnostic job")
	ErrDuplicateJob         = errors.New("diagnostic job already exists for agent")
	ErrInvalidSignature     = errors.New("invalid observation signature")
	ErrStaleObservation     = errors.New("stale observation")
	ErrStaleGeneration      = errors.New("stale configuration generation")
	ErrBindingMismatch      = errors.New("observation binding mismatch")
	ErrReplayObservation    = errors.New("replayed observation")
	ErrDuplicateObservation = errors.New("duplicate observation")
	ErrSessionExpired       = errors.New("diagnostic session expired")
	ErrSessionCancelled     = errors.New("diagnostic session cancelled")
	ErrInvalidTransition    = errors.New("invalid diagnostic state transition")
)

type ManagerConfig struct {
	SessionTTL          time.Duration
	ObservationMaxAge   time.Duration
	ClockSkew           time.Duration
	MaxSessions         int
	MaxAgentsPerSession int
	VerifyObservation   VerifyObservationFunc

	// Now and NewID are injectable for deterministic tests. Production callers
	// should leave them nil.
	Now   func() time.Time
	NewID func(prefix string) (string, error)
}

type jobReference struct {
	sessionID string
	index     int
}

// DiagnosticSessionManager owns only ephemeral diagnostic state. Its config
// deliberately contains no callbacks to availability, node archive, incidents,
// Telegram, speedtest, Remnawave, subscription, or backup owners.
type DiagnosticSessionManager struct {
	mu             sync.RWMutex
	config         ManagerConfig
	sessions       map[string]*DiagnosticSession
	jobs           map[string]jobReference
	usedNonces     map[string]struct{}
	reservedNonces map[string]string
}

func NewDiagnosticSessionManager(config ManagerConfig) (*DiagnosticSessionManager, error) {
	if config.VerifyObservation == nil {
		return nil, fmt.Errorf("%w: observation verifier is required", ErrInvalidRequest)
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = DefaultSessionTTL
	}
	if config.ObservationMaxAge == 0 {
		config.ObservationMaxAge = DefaultObservationMaxAge
	}
	if config.ClockSkew == 0 {
		config.ClockSkew = DefaultClockSkew
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = DefaultMaxSessions
	}
	if config.MaxAgentsPerSession == 0 {
		config.MaxAgentsPerSession = DefaultMaxAgentsPerSession
	}
	if config.SessionTTL < 0 || config.ObservationMaxAge < 0 || config.ClockSkew < 0 || config.MaxSessions < 1 || config.MaxAgentsPerSession < 1 {
		return nil, fmt.Errorf("%w: manager limits must be positive", ErrInvalidRequest)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.NewID == nil {
		config.NewID = randomID
	}
	return &DiagnosticSessionManager{
		config:         config,
		sessions:       make(map[string]*DiagnosticSession),
		jobs:           make(map[string]jobReference),
		usedNonces:     make(map[string]struct{}),
		reservedNonces: make(map[string]string),
	}, nil
}

func (m *DiagnosticSessionManager) CreateSession(request CreateSessionRequest) (DiagnosticSession, error) {
	now := m.now()
	request.StableID = strings.TrimSpace(request.StableID)
	request.ConfigFingerprint = strings.TrimSpace(request.ConfigFingerprint)
	request.AutomationContext.Kind = strings.TrimSpace(request.AutomationContext.Kind)
	request.AutomationContext.Outcome = strings.TrimSpace(request.AutomationContext.Outcome)
	request.AutomationContext.Source = strings.TrimSpace(request.AutomationContext.Source)
	agents, err := normalizedAgentIDs(request.RequestedAgents)
	if err != nil {
		return DiagnosticSession{}, err
	}
	request.RequestedAgents = agents
	if err := validateCreateSessionRequest(request, now, m.config.SessionTTL, m.config.MaxAgentsPerSession); err != nil {
		return DiagnosticSession{}, err
	}

	sessionID, err := m.config.NewID("diag")
	if err != nil {
		return DiagnosticSession{}, fmt.Errorf("generate session ID: %w", err)
	}
	if !validToken(sessionID) {
		return DiagnosticSession{}, fmt.Errorf("%w: generated session ID is invalid", ErrInvalidRequest)
	}
	expiresAt := request.ExpiresAt.UTC()
	if expiresAt.IsZero() {
		expiresAt = now.Add(m.config.SessionTTL)
	}
	session := &DiagnosticSession{
		SchemaVersion:         SessionSchemaVersion,
		SessionID:             sessionID,
		StableID:              request.StableID,
		Trigger:               request.Trigger,
		ConfigGeneration:      request.ConfigGeneration,
		ConfigFingerprint:     request.ConfigFingerprint,
		LocalResultSnapshot:   request.LocalResultSnapshot,
		AutomationContext:     request.AutomationContext,
		RequestedAgents:       append([]string(nil), request.RequestedAgents...),
		MaintenanceDiagnostic: request.MaintenanceDiagnostic,
		CreatedAt:             now,
		ExpiresAt:             expiresAt,
		State:                 SessionStateRequested,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	if _, exists := m.sessions[sessionID]; exists {
		return DiagnosticSession{}, fmt.Errorf("%w: duplicate generated session ID", ErrInvalidRequest)
	}
	if err := m.makeCapacityLocked(); err != nil {
		return DiagnosticSession{}, err
	}
	m.sessions[sessionID] = session
	return cloneSession(*session), nil
}

func (m *DiagnosticSessionManager) RegisterJob(request RegisterJobRequest) (DiagnosticJob, error) {
	now := m.now()
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.AgentID = strings.TrimSpace(request.AgentID)
	if !validToken(request.SessionID) || !validToken(request.AgentID) {
		return DiagnosticJob{}, fmt.Errorf("%w: sessionId and agentId are required", ErrInvalidRequest)
	}
	if err := validateProfile(request.Profile); err != nil {
		return DiagnosticJob{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	session, ok := m.sessions[request.SessionID]
	if !ok {
		return DiagnosticJob{}, ErrUnknownSession
	}
	if err := activeSessionError(session.State); err != nil {
		return DiagnosticJob{}, err
	}
	if !contains(session.RequestedAgents, request.AgentID) {
		return DiagnosticJob{}, fmt.Errorf("%w: agent was not requested", ErrInvalidRequest)
	}
	for _, job := range session.Jobs {
		if job.AgentID == request.AgentID {
			return DiagnosticJob{}, ErrDuplicateJob
		}
	}

	jobID, err := m.config.NewID("job")
	if err != nil {
		return DiagnosticJob{}, fmt.Errorf("generate job ID: %w", err)
	}
	nonce, err := m.config.NewID("nonce")
	if err != nil {
		return DiagnosticJob{}, fmt.Errorf("generate job nonce: %w", err)
	}
	if !validToken(jobID) || !validToken(nonce) {
		return DiagnosticJob{}, fmt.Errorf("%w: generated job binding is invalid", ErrInvalidRequest)
	}
	if _, exists := m.jobs[jobID]; exists {
		return DiagnosticJob{}, fmt.Errorf("%w: duplicate generated job ID", ErrInvalidRequest)
	}
	nonceKey := request.AgentID + "\x00" + nonce
	if _, exists := m.reservedNonces[nonceKey]; exists {
		return DiagnosticJob{}, fmt.Errorf("%w: duplicate generated job nonce", ErrInvalidRequest)
	}
	expiresAt := request.ExpiresAt.UTC()
	if expiresAt.IsZero() {
		expiresAt = session.ExpiresAt
	}
	if !expiresAt.After(now) || expiresAt.After(session.ExpiresAt) {
		return DiagnosticJob{}, fmt.Errorf("%w: job expiry must be within the session deadline", ErrInvalidRequest)
	}
	job := DiagnosticJob{
		SchemaVersion:     JobSchemaVersion,
		JobID:             jobID,
		SessionID:         session.SessionID,
		AgentID:           request.AgentID,
		Nonce:             nonce,
		StableID:          session.StableID,
		ConfigGeneration:  session.ConfigGeneration,
		ConfigFingerprint: session.ConfigFingerprint,
		Profile:           request.Profile,
		CreatedAt:         now,
		ExpiresAt:         expiresAt,
		State:             JobStatePending,
	}
	session.Jobs = append(session.Jobs, job)
	m.jobs[jobID] = jobReference{sessionID: session.SessionID, index: len(session.Jobs) - 1}
	m.reservedNonces[nonceKey] = jobID
	m.deriveSessionStateLocked(session, now)
	return job, nil
}

func (m *DiagnosticSessionManager) MarkJobDispatched(jobID string) error {
	return m.transitionJob(jobID, JobStateDispatched)
}

func (m *DiagnosticSessionManager) MarkJobRunning(jobID string) error {
	return m.transitionJob(jobID, JobStateRunning)
}

func (m *DiagnosticSessionManager) transitionJob(jobID string, next JobState) error {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	session, job, err := m.jobLocked(strings.TrimSpace(jobID))
	if err != nil {
		return err
	}
	if err := activeSessionError(session.State); err != nil {
		return err
	}
	switch next {
	case JobStateDispatched:
		if job.State != JobStatePending {
			return ErrInvalidTransition
		}
	case JobStateRunning:
		if job.State != JobStatePending && job.State != JobStateDispatched {
			return ErrInvalidTransition
		}
	default:
		return ErrInvalidTransition
	}
	job.State = next
	m.deriveSessionStateLocked(session, now)
	return nil
}

func (m *DiagnosticSessionManager) AcceptObservation(observation Observation) (AcceptedObservation, error) {
	now := m.now()
	if err := validateObservation(observation); err != nil {
		return AcceptedObservation{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	session, job, err := m.jobLocked(observation.JobID)
	if err != nil {
		return AcceptedObservation{}, err
	}
	if err := validateObservationTime(observation.CheckedAt, now, job.CreatedAt, job.ExpiresAt, m.config.ObservationMaxAge, m.config.ClockSkew); err != nil {
		return AcceptedObservation{}, err
	}
	payload, err := ObservationSigningPayload(observation)
	if err != nil {
		return AcceptedObservation{}, fmt.Errorf("build observation signing payload: %w", err)
	}
	if err := m.config.VerifyObservation(observation.AgentID, payload, observation.Signature); err != nil {
		return AcceptedObservation{}, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if job.State == JobStateCompleted {
		return AcceptedObservation{}, ErrDuplicateObservation
	}
	if err := activeSessionError(session.State); err != nil {
		return AcceptedObservation{}, err
	}
	nonceKey := observation.AgentID + "\x00" + observation.Nonce
	if _, used := m.usedNonces[nonceKey]; used {
		return AcceptedObservation{}, ErrReplayObservation
	}
	if observation.ConfigGeneration != job.ConfigGeneration || observation.ConfigGeneration != session.ConfigGeneration {
		return AcceptedObservation{}, ErrStaleGeneration
	}
	if observation.SchemaVersion != ObservationSchemaVersion ||
		observation.SessionID != job.SessionID ||
		observation.AgentID != job.AgentID ||
		observation.Nonce != job.Nonce ||
		observation.StableID != job.StableID ||
		observation.ConfigFingerprint != job.ConfigFingerprint ||
		observation.EndpointProfile != job.Profile.ID {
		return AcceptedObservation{}, ErrBindingMismatch
	}
	if observation.AlternativeEndpoint != nil {
		if job.Profile.AlternativeProfileID == "" || observation.AlternativeEndpoint.ProfileID != job.Profile.AlternativeProfileID {
			return AcceptedObservation{}, ErrBindingMismatch
		}
	}

	record := AcceptedObservation{
		Observation: cloneObservation(observation),
		AcceptedAt:  now,
		Reliable:    observation.DirectConnectivity.Checked && observation.DirectConnectivity.Online,
	}
	session.AgentObservations = append(session.AgentObservations, record)
	job.State = JobStateCompleted
	m.usedNonces[nonceKey] = struct{}{}
	m.deriveSessionStateLocked(session, now)
	return cloneAcceptedObservation(record), nil
}

// DeleteSession discards one session and everything bound to it. Cancelling only
// stops a session; the evidence stays on screen until it ages out, which is why
// an operator needs a way to clear a finished run explicitly.
func (m *DiagnosticSessionManager) DeleteSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return ErrUnknownSession
	}
	m.removeSessionLocked(sessionID)
	return nil
}

// DeleteSessions discards every session for one node, or all of them when
// stableID is empty. It returns how many were removed.
func (m *DiagnosticSessionManager) DeleteSessions(stableID string) int {
	stableID = strings.TrimSpace(stableID)
	m.mu.Lock()
	defer m.mu.Unlock()
	doomed := make([]string, 0, len(m.sessions))
	for sessionID, session := range m.sessions {
		if stableID == "" || session.StableID == stableID {
			doomed = append(doomed, sessionID)
		}
	}
	for _, sessionID := range doomed {
		m.removeSessionLocked(sessionID)
	}
	return len(doomed)
}

func (m *DiagnosticSessionManager) CancelSession(sessionID string) error {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	session, ok := m.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return ErrUnknownSession
	}
	if session.State.Terminal() {
		return ErrInvalidTransition
	}
	for index := range session.Jobs {
		if !session.Jobs[index].State.Terminal() {
			session.Jobs[index].State = JobStateCancelled
		}
	}
	session.State = SessionStateCancelled
	return nil
}

func (m *DiagnosticSessionManager) Session(sessionID string) (DiagnosticSession, bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	session, ok := m.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return DiagnosticSession{}, false
	}
	return cloneSession(*session), true
}

func (m *DiagnosticSessionManager) Sessions() []DiagnosticSession {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	result := make([]DiagnosticSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		result = append(result, cloneSession(*session))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// ExportSession returns only the isolated diagnostic schema. Execution config,
// proxy credentials, and arbitrary transport error text never enter this data.
func (m *DiagnosticSessionManager) ExportSession(sessionID string) ([]byte, error) {
	session, ok := m.Session(sessionID)
	if !ok {
		return nil, ErrUnknownSession
	}
	return json.MarshalIndent(session, "", "  ")
}

func (m *DiagnosticSessionManager) now() time.Time {
	return m.config.Now().UTC()
}

func (m *DiagnosticSessionManager) jobLocked(jobID string) (*DiagnosticSession, *DiagnosticJob, error) {
	reference, ok := m.jobs[jobID]
	if !ok {
		return nil, nil, ErrUnknownJob
	}
	session, ok := m.sessions[reference.sessionID]
	if !ok || reference.index < 0 || reference.index >= len(session.Jobs) {
		return nil, nil, ErrUnknownJob
	}
	return session, &session.Jobs[reference.index], nil
}

func (m *DiagnosticSessionManager) expireLocked(now time.Time) {
	for _, session := range m.sessions {
		if session.State.Terminal() {
			continue
		}
		for index := range session.Jobs {
			job := &session.Jobs[index]
			if !job.State.Terminal() && !job.ExpiresAt.After(now) {
				job.State = JobStateExpired
			}
		}
		m.deriveSessionStateLocked(session, now)
	}
}

func (m *DiagnosticSessionManager) deriveSessionStateLocked(session *DiagnosticSession, now time.Time) {
	if session.State == SessionStateCancelled {
		return
	}
	completed := 0
	terminal := 0
	running := false
	for _, job := range session.Jobs {
		switch job.State {
		case JobStateCompleted:
			completed++
			terminal++
		case JobStateExpired, JobStateCancelled:
			terminal++
		case JobStateRunning:
			running = true
		}
	}
	allRequestedCompleted := len(session.RequestedAgents) > 0 && completed == len(session.RequestedAgents)
	deadlineReached := !session.ExpiresAt.After(now)
	allRegisteredTerminal := len(session.Jobs) == len(session.RequestedAgents) && terminal == len(session.Jobs)
	if allRequestedCompleted {
		session.State = SessionStateCompleted
		return
	}
	if deadlineReached || allRegisteredTerminal {
		for index := range session.Jobs {
			if !session.Jobs[index].State.Terminal() {
				session.Jobs[index].State = JobStateExpired
			}
		}
		if completed > 0 {
			session.State = SessionStatePartial
		} else {
			session.State = SessionStateExpired
		}
		return
	}
	if running || completed > 0 {
		session.State = SessionStateRunning
		return
	}
	if len(session.Jobs) > 0 {
		session.State = SessionStateDispatching
		return
	}
	session.State = SessionStateRequested
}

func (m *DiagnosticSessionManager) makeCapacityLocked() error {
	if len(m.sessions) < m.config.MaxSessions {
		return nil
	}
	type terminalSession struct {
		id        string
		createdAt time.Time
	}
	var terminal []terminalSession
	for id, session := range m.sessions {
		if session.State.Terminal() {
			terminal = append(terminal, terminalSession{id: id, createdAt: session.CreatedAt})
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].createdAt.Before(terminal[j].createdAt)
	})
	for _, candidate := range terminal {
		m.removeSessionLocked(candidate.id)
		if len(m.sessions) < m.config.MaxSessions {
			return nil
		}
	}
	return ErrCapacity
}

func (m *DiagnosticSessionManager) removeSessionLocked(sessionID string) {
	session, ok := m.sessions[sessionID]
	if !ok {
		return
	}
	for _, job := range session.Jobs {
		delete(m.jobs, job.JobID)
		delete(m.reservedNonces, job.AgentID+"\x00"+job.Nonce)
	}
	for _, observation := range session.AgentObservations {
		delete(m.usedNonces, observation.Observation.AgentID+"\x00"+observation.Observation.Nonce)
	}
	delete(m.sessions, sessionID)
}

func validateCreateSessionRequest(request CreateSessionRequest, now time.Time, maxTTL time.Duration, maxAgents int) error {
	if !validToken(request.StableID) {
		return fmt.Errorf("%w: stableId is required", ErrInvalidRequest)
	}
	switch request.Trigger {
	case TriggerManual, TriggerAutoProxyFailure, TriggerAutoCheckEndpoint, TriggerAutoAmbiguousFailure, TriggerAutoSpeedFallback:
	default:
		return fmt.Errorf("%w: unsupported trigger", ErrInvalidRequest)
	}
	if request.MaintenanceDiagnostic && request.Trigger.Automatic() {
		return fmt.Errorf("%w: maintenance diagnostics must be manual", ErrInvalidRequest)
	}
	if request.Trigger == TriggerAutoSpeedFallback {
		context := request.AutomationContext
		if context.Kind != AutomationKindSpeedFallback ||
			(context.Outcome != AutomationOutcomeTechnical && context.Outcome != AutomationOutcomeLowSpeed) ||
			!validToken(context.Source) || context.ThresholdMbps < 0 || context.ObservedMbps < 0 || context.FallbackAttempts < 1 {
			return fmt.Errorf("%w: invalid speed fallback automation context", ErrInvalidRequest)
		}
	} else if request.AutomationContext != (AutomationContext{}) {
		return fmt.Errorf("%w: automation context does not match trigger", ErrInvalidRequest)
	}
	if !validFingerprint(request.ConfigFingerprint) {
		return fmt.Errorf("%w: config fingerprint must be sha256", ErrInvalidRequest)
	}
	if len(request.RequestedAgents) == 0 {
		return fmt.Errorf("%w: at least one agent is required", ErrInvalidRequest)
	}
	if len(request.RequestedAgents) > maxAgents {
		return fmt.Errorf("%w: requested agents exceed configured limit", ErrInvalidRequest)
	}
	if err := validateLocalResultSnapshot(request.LocalResultSnapshot); err != nil {
		return err
	}
	if request.ExpiresAt.IsZero() {
		return nil
	}
	expiresAt := request.ExpiresAt.UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(maxTTL)) {
		return fmt.Errorf("%w: session expiry exceeds configured TTL", ErrInvalidRequest)
	}
	return nil
}

func validateLocalResultSnapshot(snapshot LocalResultSnapshot) error {
	if snapshot.LatencyMillis < 0 {
		return fmt.Errorf("%w: local latency cannot be negative", ErrInvalidRequest)
	}
	switch snapshot.Status {
	case ProbeStatusUnknown:
		if snapshot.Failure.Code != "" || snapshot.Failure.Stage != "" {
			if !validFailureCode(snapshot.Failure.Code) || !validFailureStage(snapshot.Failure.Stage) {
				return fmt.Errorf("%w: invalid local failure classification", ErrInvalidRequest)
			}
		}
	case ProbeStatusOnline, ProbeStatusProxyFailure, ProbeStatusOffline:
		if snapshot.CheckedAt.IsZero() {
			return fmt.Errorf("%w: checkedAt is required for a known local result", ErrInvalidRequest)
		}
		if err := validateProbeResult(snapshot.Status, snapshot.Failure); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: invalid local probe status", ErrInvalidRequest)
	}
	for _, evidence := range []CheckEvidence{snapshot.TCP, snapshot.Ping} {
		if err := validateCheckEvidence(evidence); err != nil {
			return fmt.Errorf("%w: invalid local connectivity evidence", ErrInvalidRequest)
		}
	}
	return nil
}

func validateObservation(observation Observation) error {
	if observation.SchemaVersion != ObservationSchemaVersion {
		return ErrInvalidSchema
	}
	for name, value := range map[string]string{
		"agentId":         observation.AgentID,
		"sessionId":       observation.SessionID,
		"jobId":           observation.JobID,
		"nonce":           observation.Nonce,
		"stableId":        observation.StableID,
		"endpointProfile": observation.EndpointProfile,
		"agentVersion":    observation.AgentVersion,
	} {
		if !validToken(value) {
			return fmt.Errorf("%w: %s is invalid", ErrInvalidRequest, name)
		}
	}
	if !validFingerprint(observation.ConfigFingerprint) {
		return fmt.Errorf("%w: config fingerprint must be sha256", ErrInvalidRequest)
	}
	if observation.CheckedAt.IsZero() || observation.DurationMillis < 0 || observation.LatencyMillis < 0 {
		return fmt.Errorf("%w: invalid observation timing", ErrInvalidRequest)
	}
	if len(observation.Signature) == 0 || len(observation.Signature) > MaxObservationSignatureBytes {
		return fmt.Errorf("%w: signature is required", ErrInvalidSignature)
	}
	if !observation.DirectConnectivity.Checked {
		return fmt.Errorf("%w: direct connectivity control is required", ErrInvalidRequest)
	}
	if err := validateProbeResult(observation.Status, observation.Failure); err != nil {
		return err
	}
	for _, evidence := range []CheckEvidence{observation.TCP, observation.Ping, observation.DirectConnectivity} {
		if err := validateCheckEvidence(evidence); err != nil {
			return fmt.Errorf("%w: invalid connectivity evidence", ErrInvalidRequest)
		}
	}
	if observation.AlternativeEndpoint != nil {
		alternative := observation.AlternativeEndpoint
		if !validProfileID(alternative.ProfileID) || alternative.LatencyMillis < 0 {
			return fmt.Errorf("%w: invalid alternative endpoint result", ErrInvalidRequest)
		}
		if err := validateProbeResult(alternative.Status, alternative.Failure); err != nil {
			return err
		}
	}
	return nil
}

func validateCheckEvidence(evidence CheckEvidence) error {
	if evidence.LatencyMillis < 0 {
		return ErrInvalidRequest
	}
	if !evidence.Checked {
		if evidence.Online || evidence.LatencyMillis != 0 || evidence.FailureCode != "" {
			return ErrInvalidRequest
		}
		return nil
	}
	if evidence.Online {
		if evidence.FailureCode != "" {
			return ErrInvalidRequest
		}
		return nil
	}
	if !validFailureCode(evidence.FailureCode) {
		return ErrInvalidRequest
	}
	return nil
}

func validateProbeResult(status ProbeStatus, failure FailureEvidence) error {
	switch status {
	case ProbeStatusOnline:
		if failure.Code != "" || failure.Stage != "" {
			return fmt.Errorf("%w: online result cannot contain failure", ErrInvalidRequest)
		}
	case ProbeStatusProxyFailure, ProbeStatusOffline:
		if !validFailureCode(failure.Code) || !validFailureStage(failure.Stage) {
			return fmt.Errorf("%w: failed result requires classified failure", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: invalid probe status", ErrInvalidRequest)
	}
	return nil
}

func validateObservationTime(checkedAt, now, jobCreatedAt, jobExpiresAt time.Time, maxAge, clockSkew time.Duration) error {
	checkedAt = checkedAt.UTC()
	if checkedAt.Before(now.Add(-maxAge)) || checkedAt.After(now.Add(clockSkew)) {
		return ErrStaleObservation
	}
	if checkedAt.Before(jobCreatedAt.Add(-clockSkew)) || checkedAt.After(jobExpiresAt.Add(clockSkew)) {
		return ErrStaleObservation
	}
	return nil
}

func validateProfile(profile TestProfile) error {
	if !validProfileID(profile.ID) {
		return fmt.Errorf("%w: profile ID is invalid", ErrInvalidRequest)
	}
	if !profile.Method.Valid() {
		return fmt.Errorf("%w: unsupported probe method %q", ErrInvalidRequest, profile.Method)
	}
	if profile.AlternativeProfileID != "" {
		if !validProfileID(profile.AlternativeProfileID) || profile.AlternativeProfileID == profile.ID {
			return fmt.Errorf("%w: alternative profile ID is invalid", ErrInvalidRequest)
		}
	}
	return nil
}

func normalizedAgentIDs(agentIDs []string) ([]string, error) {
	result := make([]string, 0, len(agentIDs))
	seen := make(map[string]struct{}, len(agentIDs))
	for _, rawAgentID := range agentIDs {
		agentID := strings.TrimSpace(rawAgentID)
		if !validToken(agentID) {
			return nil, fmt.Errorf("%w: agentId is invalid", ErrInvalidRequest)
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		result = append(result, agentID)
	}
	return result, nil
}

func activeSessionError(state SessionState) error {
	switch state {
	case SessionStateExpired, SessionStatePartial:
		return ErrSessionExpired
	case SessionStateCancelled:
		return ErrSessionCancelled
	case SessionStateCompleted:
		return ErrInvalidTransition
	default:
		return nil
	}
}

func validFingerprint(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil && value == strings.ToLower(value)
}

func validProfileID(value string) bool {
	return validSafeValue(value, 64)
}

func validFailureCode(value string) bool {
	switch value {
	case "configuration",
		"dns",
		"tcp_refused",
		"tcp_timeout",
		"host_unreachable",
		"network_unreachable",
		"proxy_handshake",
		"proxy_timeout",
		"tls",
		// Reported by the TLS profile, which distinguishes how a handshake dies:
		// a reset, a silent close and an alert point at different causes.
		"tls_timeout",
		"tls_reset",
		"tls_eof",
		"tls_alert",
		"tls_failed",
		// Reported by the DNS profile.
		"dns_mismatch",
		"dns_literal_address",
		"http_status",
		"source_ip_unchanged",
		"download_incomplete",
		"check_endpoint",
		"unknown":
		return true
	default:
		return false
	}
}

func validToken(value string) bool {
	return validSafeValue(value, 128)
}

func validSafeValue(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validFailureStage(stage FailureStage) bool {
	switch stage {
	case FailureStageConfiguration, FailureStageXrayStart, FailureStageProxy, FailureStageTCP, FailureStagePing, FailureStageDirect, FailureStageEndpoint:
		return true
	default:
		return false
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func randomID(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(entropy[:]), nil
}

func cloneSession(session DiagnosticSession) DiagnosticSession {
	session.RequestedAgents = append([]string(nil), session.RequestedAgents...)
	session.Jobs = append([]DiagnosticJob(nil), session.Jobs...)
	session.AgentObservations = append([]AcceptedObservation(nil), session.AgentObservations...)
	for index := range session.AgentObservations {
		session.AgentObservations[index] = cloneAcceptedObservation(session.AgentObservations[index])
	}
	return session
}

func cloneAcceptedObservation(record AcceptedObservation) AcceptedObservation {
	record.Observation = cloneObservation(record.Observation)
	return record
}

func cloneObservation(observation Observation) Observation {
	observation.Signature = append([]byte(nil), observation.Signature...)
	if observation.AlternativeEndpoint != nil {
		alternative := *observation.AlternativeEndpoint
		observation.AlternativeEndpoint = &alternative
	}
	return observation
}
