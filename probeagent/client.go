package probeagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"xray-checker/diagnostics"
)

const IdentityVersion = 1

const maxControlResponseBytes = MaxExecutionConfigBytes + 64*1024

type JobExecutor interface {
	Execute(context.Context, JobAssignment) diagnostics.Observation
}

// ControllerRejection is a controller response the agent understood. Keeping the
// status code lets the caller separate "this job was refused" from "this agent
// is no longer allowed to talk to the controller", which need opposite reactions.
type ControllerRejection struct {
	StatusCode int
	Message    string
}

func (e *ControllerRejection) Error() string {
	return fmt.Sprintf("probe controller rejected request: %s", e.Message)
}

// JobScoped reports a refusal that concerns one job only. A stale generation, a
// schema the controller no longer accepts or a duplicate observation all mean
// this job is dead; the control connection behind it is still healthy, and
// tearing it down would stop the heartbeat that reports the agent as alive.
func (e *ControllerRejection) JobScoped() bool {
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

type ClientConfig struct {
	AgentID          string
	EnrollmentToken  string
	ControllerURL    string
	ControllerIP     string
	ControllerCAFile string
	IdentityDir      string
	AgentVersion     string
	Capabilities     []string
	RequestTimeout   time.Duration
	JobPollInterval  time.Duration
	Executor         JobExecutor
	Now              func() time.Time
	// Hooks report enrollment, heartbeat and per-job progress so the binary can
	// log them. Every field is optional.
	Hooks Hooks
}

type IdentityFile struct {
	Version               int    `json:"version"`
	AgentID               string `json:"agentId"`
	IdentityPrivateKey    string `json:"identityPrivateKey"`
	ObservationPrivateKey string `json:"observationPrivateKey"`
	Enrolled              bool   `json:"enrolled"`
	NextSequence          uint64 `json:"nextSequence"`
}

type Client struct {
	config       ClientConfig
	httpClient   *http.Client
	identityPath string
	identity     IdentityFile
	requestMu    sync.Mutex
	refusedMu    sync.Mutex
	// The controller redelivers a job until it expires, so a job whose
	// observation was refused would otherwise be re-executed on every poll:
	// a full Xray start and probe every few seconds, for nothing.
	refusedJobs map[string]struct{}
}

type apiEnvelope[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data"`
	Error   string `json:"error"`
}

func NewClient(config ClientConfig) (*Client, error) {
	config.AgentID = strings.TrimSpace(config.AgentID)
	config.ControllerURL = strings.TrimRight(strings.TrimSpace(config.ControllerURL), "/")
	config.ControllerIP = normalizeIP(config.ControllerIP)
	config.IdentityDir = strings.TrimSpace(config.IdentityDir)
	config.AgentVersion = strings.TrimSpace(config.AgentVersion)
	config.Capabilities = normalizeCapabilities(config.Capabilities)
	if !validIdentifier(config.AgentID, 96) || config.IdentityDir == "" || !validShortText(config.AgentVersion, 128) {
		return nil, fmt.Errorf("invalid probe agent client identity configuration")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	if config.RequestTimeout < time.Second {
		return nil, fmt.Errorf("probe agent request timeout is too short")
	}
	if config.JobPollInterval == 0 {
		config.JobPollInterval = 5 * time.Second
	}
	if config.JobPollInterval < time.Second {
		return nil, fmt.Errorf("probe agent job poll interval is too short")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	httpClient, err := newPinnedHTTPClient(config.ControllerURL, config.ControllerIP, config.ControllerCAFile, config.RequestTimeout)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.IdentityDir, 0700); err != nil {
		return nil, fmt.Errorf("create probe agent identity directory: %w", err)
	}
	client := &Client{
		config:       config,
		httpClient:   httpClient,
		identityPath: filepath.Join(config.IdentityDir, "identity.json"),
	}
	if err := client.loadOrCreateIdentity(); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *Client) EnsureEnrolled(ctx context.Context) (int, error) {
	if c.identity.Enrolled {
		return DefaultHeartbeatIntervalSec, nil
	}
	if strings.TrimSpace(c.config.EnrollmentToken) == "" {
		return 0, fmt.Errorf("probe enrollment token is required until the first successful enrollment")
	}
	identityPrivate, observationPrivate, err := c.privateKeys()
	if err != nil {
		return 0, err
	}
	request := EnrollRequest{
		ProtocolVersion:      ProtocolVersion,
		AgentID:              c.config.AgentID,
		EnrollmentToken:      c.config.EnrollmentToken,
		IdentityPublicKey:    append([]byte(nil), identityPrivate.Public().(ed25519.PublicKey)...),
		ObservationPublicKey: append([]byte(nil), observationPrivate.Public().(ed25519.PublicKey)...),
		AgentVersion:         c.config.AgentVersion,
		Capabilities:         c.config.Capabilities,
	}
	var response EnrollResponse
	if err := c.postJSON(ctx, EnrollPath, request, nil, &response); err != nil {
		// If the controller consumed the token but the container restarted before
		// persisting Enrolled=true, the already persisted identity can recover by
		// authenticating a heartbeat instead of requiring another token.
		if _, heartbeatErr := c.SendHeartbeat(ctx, "starting"); heartbeatErr == nil {
			c.identity.Enrolled = true
			if persistErr := c.persistIdentity(); persistErr != nil {
				return 0, persistErr
			}
			c.notifyEnrolled(DefaultHeartbeatIntervalSec, true)
			return DefaultHeartbeatIntervalSec, nil
		}
		return 0, err
	}
	c.identity.Enrolled = true
	if err := c.persistIdentity(); err != nil {
		return 0, err
	}
	if response.HeartbeatInterval < 5 {
		response.HeartbeatInterval = DefaultHeartbeatIntervalSec
	}
	c.notifyEnrolled(response.HeartbeatInterval, false)
	return response.HeartbeatInterval, nil
}

func (c *Client) notifyEnrolled(intervalSeconds int, resumed bool) {
	if c.config.Hooks.OnEnrolled != nil {
		c.config.Hooks.OnEnrolled(intervalSeconds, resumed)
	}
}

func (c *Client) SendHeartbeat(ctx context.Context, health string) (HeartbeatResponse, error) {
	if !validHealth(health) {
		return HeartbeatResponse{}, fmt.Errorf("invalid probe agent health value")
	}
	request := HeartbeatRequest{
		ProtocolVersion: ProtocolVersion,
		AgentID:         c.config.AgentID,
		AgentVersion:    c.config.AgentVersion,
		Capabilities:    c.config.Capabilities,
		Health:          health,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	var response HeartbeatResponse
	if err := c.postSignedJSONBytes(ctx, HeartbeatPath, body, &response); err != nil {
		return HeartbeatResponse{}, err
	}
	return response, nil
}

func (c *Client) Run(ctx context.Context) error {
	intervalSeconds, err := c.EnsureEnrolled(ctx)
	if err != nil {
		return err
	}
	if _, err := c.SendHeartbeat(ctx, "healthy"); err != nil {
		return err
	}
	if c.config.Hooks.OnConnected != nil {
		c.config.Hooks.OnConnected(intervalSeconds)
	}
	if c.config.Executor == nil {
		return c.runHeartbeatLoop(ctx, time.Duration(intervalSeconds)*time.Second)
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- c.runHeartbeatLoop(runContext, time.Duration(intervalSeconds)*time.Second) }()
	go func() { errorsChannel <- c.runJobLoop(runContext) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsChannel:
		return err
	}
}

func (c *Client) runHeartbeatLoop(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.SendHeartbeat(ctx, "healthy"); err != nil {
				if c.config.Hooks.OnHeartbeatFailed != nil {
					c.config.Hooks.OnHeartbeatFailed(err)
				}
				return err
			}
		}
	}
}

func (c *Client) runJobLoop(ctx context.Context) error {
	ticker := time.NewTicker(c.config.JobPollInterval)
	defer ticker.Stop()
	for {
		if err := c.pollAndExecute(ctx); err != nil {
			// A refused job used to tear down the whole control connection, so
			// one rejected observation stopped the heartbeat for the length of
			// the reconnect backoff and reported a healthy agent as offline.
			var rejection *ControllerRejection
			if !errors.As(err, &rejection) || !rejection.JobScoped() {
				return err
			}
			if c.config.Hooks.OnJobRejected != nil {
				c.config.Hooks.OnJobRejected(err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Client) pollAndExecute(ctx context.Context) error {
	request := ControlRequest{ProtocolVersion: ProtocolVersion, AgentID: c.config.AgentID}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var response JobPollResponse
	if err := c.postSignedJSONBytes(ctx, JobPollPath, body, &response); err != nil {
		return err
	}
	if response.Job == nil {
		return nil
	}
	assignment := *response.Job
	if assignment.Job.AgentID != c.config.AgentID || !assignment.Job.ExpiresAt.After(c.config.Now().UTC()) {
		return fmt.Errorf("probe controller returned an invalid job binding")
	}
	if c.jobRefused(assignment.Job.JobID) {
		return nil
	}
	if c.config.Hooks.OnJobStarted != nil {
		c.config.Hooks.OnJobStarted(jobStartedFrom(assignment, c.config.Now().UTC()))
	}
	startedAt := c.config.Now()
	jobContext, cancel := context.WithDeadline(ctx, assignment.Job.ExpiresAt)
	observation := c.config.Executor.Execute(jobContext, assignment)
	cancel()
	observation.AgentID = c.config.AgentID
	observation.AgentVersion = c.config.AgentVersion
	finished := jobFinishedFrom(observation, c.config.Now().Sub(startedAt))
	if c.config.Hooks.OnJobFinished != nil {
		c.config.Hooks.OnJobFinished(finished)
	}
	_, observationPrivate, err := c.privateKeys()
	if err != nil {
		return err
	}
	payload, err := diagnostics.ObservationSigningPayload(observation)
	if err != nil {
		return err
	}
	observation.Signature = ed25519.Sign(observationPrivate, payload)
	body, err = json.Marshal(observation)
	if err != nil {
		return err
	}
	var accepted ObservationResponse
	if err := c.postSignedJSONBytes(ctx, ObservationPath, body, &accepted); err != nil {
		var rejection *ControllerRejection
		if errors.As(err, &rejection) && rejection.JobScoped() {
			c.markJobRefused(assignment.Job.JobID)
		}
		return err
	}
	if c.config.Hooks.OnObservationAccepted != nil {
		c.config.Hooks.OnObservationAccepted(finished)
	}
	return nil
}

func (c *Client) jobRefused(jobID string) bool {
	c.refusedMu.Lock()
	defer c.refusedMu.Unlock()
	_, refused := c.refusedJobs[jobID]
	return refused
}

func (c *Client) markJobRefused(jobID string) {
	c.refusedMu.Lock()
	defer c.refusedMu.Unlock()
	if c.refusedJobs == nil {
		c.refusedJobs = make(map[string]struct{})
	}
	// Jobs expire on the controller within minutes, so this set only has to
	// outlive the redelivery window. Clearing it wholesale keeps the agent from
	// accumulating state it can never prune on its own.
	if len(c.refusedJobs) >= 256 {
		c.refusedJobs = make(map[string]struct{})
	}
	c.refusedJobs[jobID] = struct{}{}
}

func (c *Client) postSignedJSONBytes(ctx context.Context, path string, body []byte, output any) error {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	privateKey, _, err := c.privateKeys()
	if err != nil {
		return err
	}
	c.identity.NextSequence++
	sequence := c.identity.NextSequence
	if err := c.persistIdentity(); err != nil {
		return err
	}
	timestamp := c.config.Now().UTC()
	payload, err := ControlSigningPayload(http.MethodPost, path, c.config.AgentID, timestamp, sequence, body)
	if err != nil {
		return err
	}
	headers := http.Header{
		"X-Probe-Agent-ID":  []string{c.config.AgentID},
		"X-Probe-Timestamp": []string{timestamp.Format(time.RFC3339Nano)},
		"X-Probe-Sequence":  []string{strconv.FormatUint(sequence, 10)},
		"X-Probe-Signature": []string{base64.RawStdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))},
	}
	return c.postJSONBytes(ctx, path, body, headers, output)
}

func (c *Client) postJSON(ctx context.Context, path string, input any, headers http.Header, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return c.postJSONBytes(ctx, path, body, headers, output)
}

func (c *Client) postJSONBytes(ctx context.Context, path string, body []byte, headers http.Header, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.ControllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("probe controller request failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes))
	if err != nil {
		return fmt.Errorf("read probe controller response: %w", err)
	}
	var envelope apiEnvelope[json.RawMessage]
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("invalid probe controller response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		if envelope.Error == "" {
			envelope.Error = http.StatusText(response.StatusCode)
		}
		return &ControllerRejection{StatusCode: response.StatusCode, Message: envelope.Error}
	}
	if output != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return fmt.Errorf("decode probe controller response: %w", err)
		}
	}
	return nil
}

func (c *Client) loadOrCreateIdentity() error {
	data, err := os.ReadFile(c.identityPath)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c.identity); err != nil {
			return fmt.Errorf("decode probe agent identity: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("decode probe agent identity: trailing JSON data")
		}
		if c.identity.Version != IdentityVersion || c.identity.AgentID != c.config.AgentID {
			return fmt.Errorf("probe agent identity does not match configured agent ID")
		}
		if err := os.Chmod(c.identityPath, 0600); err != nil {
			return fmt.Errorf("protect probe agent identity: %w", err)
		}
		_, _, err := c.privateKeys()
		return err
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read probe agent identity: %w", err)
	}
	_, identityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate probe identity key: %w", err)
	}
	_, observationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate probe observation key: %w", err)
	}
	c.identity = IdentityFile{
		Version:               IdentityVersion,
		AgentID:               c.config.AgentID,
		IdentityPrivateKey:    base64.RawStdEncoding.EncodeToString(identityPrivate),
		ObservationPrivateKey: base64.RawStdEncoding.EncodeToString(observationPrivate),
	}
	return c.persistIdentity()
}

func (c *Client) privateKeys() (ed25519.PrivateKey, ed25519.PrivateKey, error) {
	identity, identityErr := base64.RawStdEncoding.DecodeString(c.identity.IdentityPrivateKey)
	observation, observationErr := base64.RawStdEncoding.DecodeString(c.identity.ObservationPrivateKey)
	if identityErr != nil || observationErr != nil || len(identity) != ed25519.PrivateKeySize || len(observation) != ed25519.PrivateKeySize || bytes.Equal(identity, observation) {
		return nil, nil, fmt.Errorf("probe agent identity file contains invalid keys")
	}
	return ed25519.PrivateKey(identity), ed25519.PrivateKey(observation), nil
}

func (c *Client) persistIdentity() error {
	data, err := json.MarshalIndent(c.identity, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(c.config.IdentityDir, ".identity-*.tmp")
	if err != nil {
		return fmt.Errorf("create probe identity temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, c.identityPath); err != nil {
		return fmt.Errorf("publish probe identity: %w", err)
	}
	return nil
}

func newPinnedHTTPClient(controllerURL, controllerIP, caFile string, timeout time.Duration) (*http.Client, error) {
	parsed, err := url.Parse(controllerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("probe controller URL must be HTTPS without credentials, query or fragment")
	}
	pinnedIP, err := netip.ParseAddr(strings.TrimSpace(controllerIP))
	if err != nil {
		return nil, fmt.Errorf("probe controller IP must be an exact IPv4 or IPv6 address")
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	serverName := parsed.Hostname()
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if strings.TrimSpace(caFile) != "" {
		certificate, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read probe controller CA: %w", err)
		}
		if !rootCAs.AppendCertsFromPEM(certificate) {
			return nil, errors.New("probe controller CA file contains no certificates")
		}
	}
	dialAddress := net.JoinHostPort(pinnedIP.Unmap().String(), port)
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, dialAddress)
		},
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, RootCAs: rootCAs},
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
