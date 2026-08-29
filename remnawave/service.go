package remnawave

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"xray-checker/checker"
	"xray-checker/logger"
	"xray-checker/models"
	"xray-checker/nodearchive"
)

type ProxySource interface {
	GetProxies() []*models.ProxyConfig
	GetProxyStatusDetailsIncludingMaintenance(string) (checker.ProxyStatusDetails, error)
	MonitoringEnabled(string) bool
}

type IncidentSource interface {
	Incidents(int) []nodearchive.IncidentRecord
}

type Options struct {
	MasterEnabled      bool
	APIURL             string
	APITokenConfigured bool
	ConfigPath         string
	RuntimePath        string
	API                API
	ProxySource        ProxySource
	IncidentSource     IncidentSource
	RequestTimeout     time.Duration
	ReconcileInterval  time.Duration
	TopologyInterval   time.Duration
}

type Service struct {
	masterEnabled      bool
	apiURL             string
	apiTokenConfigured bool
	configPath         string
	runtimePath        string
	api                API
	proxySource        ProxySource
	incidentSource     IncidentSource
	requestTimeout     time.Duration
	reconcileInterval  time.Duration
	topologyInterval   time.Duration

	operationMu sync.Mutex
	mu          sync.RWMutex
	config      ConfigFile
	runtime     RuntimeFile
	topology    Topology
	status      ReconcileStatus
	runtimeOK   bool

	failureObservations map[string]int
	recoverySince       map[string]time.Time
	now                 func() time.Time

	startStopMu sync.Mutex
	started     bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	trigger     chan struct{}
}

type desiredAnnouncement struct {
	Message             string
	KnownHealthyMessage string
	Groups              map[string]string
	PartialGroups       map[string]string
	MaintenanceGroups   map[string]string
}

type groupEvaluation struct {
	Key   string
	Label string
	State string
}

const (
	groupHealthy     = "healthy"
	groupDown        = "down"
	groupPartial     = "partial"
	groupPending     = "pending"
	groupAmbiguous   = "ambiguous"
	groupMaintenance = "maintenance"
)

func NewService(options Options) *Service {
	requestTimeout := options.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 10 * time.Second
	}
	reconcileInterval := options.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = time.Minute
	}
	topologyInterval := options.TopologyInterval
	if topologyInterval <= 0 {
		topologyInterval = 5 * time.Minute
	}
	return &Service{
		masterEnabled:       options.MasterEnabled,
		apiURL:              strings.TrimRight(strings.TrimSpace(options.APIURL), "/"),
		apiTokenConfigured:  options.APITokenConfigured,
		configPath:          options.ConfigPath,
		runtimePath:         options.RuntimePath,
		api:                 options.API,
		proxySource:         options.ProxySource,
		incidentSource:      options.IncidentSource,
		requestTimeout:      requestTimeout,
		reconcileInterval:   reconcileInterval,
		topologyInterval:    topologyInterval,
		config:              defaultConfig(),
		runtime:             defaultRuntime(),
		runtimeOK:           true,
		failureObservations: map[string]int{},
		recoverySince:       map[string]time.Time{},
		now:                 time.Now,
		trigger:             make(chan struct{}, 1),
	}
}

func (s *Service) LoadConfig() error {
	config, err := readConfigFile(s.configPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
	return nil
}

func (s *Service) LoadRuntime() error {
	runtime, err := readRuntimeFile(s.runtimePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.runtimeOK = false
		s.status.LastError = "Remnawave ownership state is invalid; remote writes are disabled: " + err.Error()
		return err
	}
	s.runtime = runtime
	s.runtimeOK = true
	return nil
}

func (s *Service) Start() {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()
	if s.started || !s.masterEnabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.started = true
	s.wg.Add(1)
	go s.loop(ctx)
}

func (s *Service) Stop() {
	s.startStopMu.Lock()
	if !s.started {
		s.startStopMu.Unlock()
		return
	}
	cancel := s.cancel
	s.started = false
	s.cancel = nil
	s.startStopMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	if _, err := s.SyncNow(ctx); err != nil {
		logger.Warn("Initial Remnawave announce sync failed: %v", err)
	}
	reconcileTicker := time.NewTicker(s.reconcileInterval)
	topologyTicker := time.NewTicker(s.topologyInterval)
	defer reconcileTicker.Stop()
	defer topologyTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			if _, err := s.ReconcileNow(ctx); err != nil {
				logger.Warn("Remnawave announce reconcile failed: %v", err)
			}
		case <-reconcileTicker.C:
			if _, err := s.ReconcileNow(ctx); err != nil {
				logger.Warn("Remnawave announce reconcile failed: %v", err)
			}
		case <-topologyTicker.C:
			if _, err := s.SyncNow(ctx); err != nil {
				logger.Warn("Remnawave announce topology sync failed: %v", err)
			}
		}
	}
}

func (s *Service) Trigger() {
	if !s.masterEnabled {
		return
	}
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// ObserveFullCheck records only completed full availability iterations. Fast
// recovery and one-off manual checks deliberately do not increment the
// confirmation counter, so a single result cannot publish an announce.
func (s *Service) ObserveFullCheck() {
	if s.proxySource == nil {
		return
	}
	proxies := s.proxySource.GetProxies()
	active := make(map[string]bool, len(proxies))
	s.mu.Lock()
	for _, proxy := range proxies {
		if proxy == nil || proxy.StableID == "" {
			continue
		}
		active[proxy.StableID] = true
		details, err := s.proxySource.GetProxyStatusDetailsIncludingMaintenance(proxy.StableID)
		if err != nil || details.Online {
			delete(s.failureObservations, proxy.StableID)
			continue
		}
		s.failureObservations[proxy.StableID]++
	}
	for stableID := range s.failureObservations {
		if !active[stableID] {
			delete(s.failureObservations, stableID)
		}
	}
	s.mu.Unlock()
	s.Trigger()
}

func (s *Service) UpdateSettings(settings Settings) (Snapshot, error) {
	s.operationMu.Lock()
	if messageScenariosMissing(settings.Policy.Messages) {
		// A v1 admin/API client sends only normalMessage. Preserve the current
		// outage templates instead of resetting them when such a client saves
		// unrelated settings, while retaining the old empty/non-empty healthy
		// message semantics.
		s.mu.RLock()
		messages := s.config.Policy.Messages
		s.mu.RUnlock()
		normalizeMessageScenarios(&messages)
		legacyNormalMessage := strings.TrimSpace(settings.Policy.NormalMessage)
		if legacyNormalMessage == "" {
			messages.Healthy.Enabled = false
		} else {
			messages.Healthy = MessageScenario{Enabled: true, Template: legacyNormalMessage}
		}
		settings.Policy.Messages = messages
		settings.Policy.NormalMessage = ""
	} else {
		if partialAvailabilityScenariosMissing(settings.Policy.Messages) {
			// A v2 admin/API client knows the original outage scenarios but not the
			// partial-redundancy fields. Preserve the current partial templates so
			// saving unrelated settings cannot reset operator customizations.
			s.mu.RLock()
			current := s.config.Policy.Messages
			s.mu.RUnlock()
			settings.Policy.Messages.PartialSingleLocation = current.PartialSingleLocation
			settings.Policy.Messages.PartialMultipleLocations = current.PartialMultipleLocations
			settings.Policy.Messages.PartialAvailabilityFallback = current.PartialAvailabilityFallback
		}
		if maintenanceScenariosMissing(settings.Policy.Messages) {
			// A v3 client predates maintenance announcements. Preserve the
			// operator's current maintenance templates on unrelated saves.
			s.mu.RLock()
			current := s.config.Policy.Messages
			s.mu.RUnlock()
			settings.Policy.Messages.MaintenanceSingleLocation = current.MaintenanceSingleLocation
			settings.Policy.Messages.MaintenanceMultipleLocations = current.MaintenanceMultipleLocations
			settings.Policy.Messages.MaintenanceFallback = current.MaintenanceFallback
			settings.Policy.Messages.MaintenanceMixedFallback = current.MaintenanceMixedFallback
		}
	}
	locations, err := canonicalSettingsLocations(settings)
	if err != nil {
		s.operationMu.Unlock()
		return Snapshot{}, err
	}
	config := ConfigFile{
		Version:    ConfigVersion,
		Policy:     settings.Policy,
		SquadPairs: append([]SquadPair(nil), settings.SquadPairs...),
		Locations:  locations,
	}
	if err := normalizeConfig(&config); err != nil {
		s.operationMu.Unlock()
		return Snapshot{}, err
	}
	if err := validateConfig(config); err != nil {
		s.operationMu.Unlock()
		return Snapshot{}, err
	}

	now := s.now().UTC()
	if err := writeConfigFile(s.configPath, config, now); err != nil {
		s.operationMu.Unlock()
		return Snapshot{}, err
	}
	config.UpdatedAt = now
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
	s.operationMu.Unlock()
	s.Trigger()
	return s.Snapshot(), nil
}

func (s *Service) SyncNow(ctx context.Context) (Snapshot, error) {
	if err := s.readyForAPI(); err != nil {
		s.setLastError(err)
		return s.Snapshot(), err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.refreshTopologyLocked(ctx); err != nil {
		s.setLastError(err)
		return s.Snapshot(), err
	}
	if err := s.reconcileLocked(ctx); err != nil {
		s.setLastError(err)
		return s.Snapshot(), err
	}
	return s.Snapshot(), nil
}

func (s *Service) ReconcileNow(ctx context.Context) (Snapshot, error) {
	if err := s.readyForAPI(); err != nil {
		s.setLastError(err)
		return s.Snapshot(), err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.reconcileLocked(ctx); err != nil {
		s.setLastError(err)
		return s.Snapshot(), err
	}
	return s.Snapshot(), nil
}

func (s *Service) readyForAPI() error {
	if !s.masterEnabled {
		return fmt.Errorf("Remnawave integration is disabled by REMNAWAVE_ANNOUNCE_ENABLED")
	}
	if s.api == nil {
		return fmt.Errorf("Remnawave API URL or token is not configured")
	}
	s.mu.RLock()
	runtimeOK := s.runtimeOK
	s.mu.RUnlock()
	if !runtimeOK {
		return fmt.Errorf("Remnawave ownership state is invalid; refusing remote writes")
	}
	return nil
}

func (s *Service) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, s.requestTimeout)
}

func (s *Service) refreshTopologyLocked(parent context.Context) error {
	ctx, cancel := s.requestContext(parent)
	hosts, err := s.api.GetHosts(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("load Remnawave hosts: %w", err)
	}
	ctx, cancel = s.requestContext(parent)
	internalSquads, err := s.api.GetInternalSquads(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("load Remnawave internal squads: %w", err)
	}
	ctx, cancel = s.requestContext(parent)
	externalSquads, err := s.api.GetExternalSquads(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("load Remnawave external squads: %w", err)
	}
	now := s.now().UTC()
	sortTopology(hosts, internalSquads, externalSquads)
	s.mu.Lock()
	s.topology = Topology{
		LoadedAt:       now,
		Hosts:          hosts,
		InternalSquads: internalSquads,
		ExternalSquads: externalSquads,
	}
	s.status.LastTopologyAt = now
	s.status.LastError = ""
	s.mu.Unlock()
	return nil
}

func (s *Service) reconcileLocked(parent context.Context) error {
	ctx, cancel := s.requestContext(parent)
	externalSquads, err := s.api.GetExternalSquads(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("refresh Remnawave external squads: %w", err)
	}
	sort.Slice(externalSquads, func(i, j int) bool {
		return strings.ToLower(externalSquads[i].Name) < strings.ToLower(externalSquads[j].Name)
	})

	s.mu.Lock()
	s.topology.ExternalSquads = cloneExternalSquads(externalSquads)
	config := s.config
	config.SquadPairs = append([]SquadPair(nil), config.SquadPairs...)
	config.Locations = cloneLocations(config.Locations)
	config.NodeMappings = cloneNodeMappings(config.NodeMappings)
	runtime := s.runtime
	runtime.Managed = cloneManaged(runtime.Managed)
	topology := cloneTopology(s.topology)
	observations := make(map[string]int, len(s.failureObservations))
	for stableID, count := range s.failureObservations {
		observations[stableID] = count
	}
	recoverySince := make(map[string]time.Time, len(s.recoverySince))
	for key, value := range s.recoverySince {
		recoverySince[key] = value
	}
	s.mu.Unlock()

	proxies, statuses, maintenance := s.proxySnapshot()
	checkEndpointIDs := s.activeCheckEndpointStableIDs()
	now := s.now().UTC()
	desired, nextRecovery, err := computeDesiredAnnouncements(
		config,
		runtime,
		topology,
		proxies,
		statuses,
		maintenance,
		observations,
		checkEndpointIDs,
		recoverySince,
		now,
	)
	if err != nil {
		return err
	}

	externalByUUID := make(map[string]ExternalSquad, len(externalSquads))
	for _, squad := range externalSquads {
		externalByUUID[squad.UUID] = squad
	}
	conflicts := make([]string, 0)
	errorsSeen := make([]string, 0)
	runtimeChanged := false
	for _, externalUUID := range sortedMapKeys(desired) {
		target := desired[externalUUID]
		squad, ok := externalByUUID[externalUUID]
		if !ok {
			errorsSeen = append(errorsSeen, fmt.Sprintf("external squad %s no longer exists", externalUUID))
			continue
		}
		currentValue, currentPresent, duplicateHeader := headerValue(squad.ResponseHeadersAdd, announceHeader)
		previous, wasManaged := runtime.Managed[externalUUID]
		owned := wasManaged && currentPresent && currentValue == previous.Value

		if duplicateHeader {
			if wasManaged {
				delete(runtime.Managed, externalUUID)
				runtimeChanged = true
			}
			conflicts = append(conflicts, fmt.Sprintf("%s: multiple case-insensitive announce headers were left untouched", squad.Name))
			continue
		}
		if wasManaged && (!currentPresent || currentValue != previous.Value) {
			delete(runtime.Managed, externalUUID)
			runtimeChanged = true
			conflicts = append(conflicts, fmt.Sprintf("%s: managed announce was changed or removed outside xray-checker and was left untouched", squad.Name))
			continue
		}

		basePresent := false
		baseValue := ""
		if wasManaged {
			basePresent = previous.BasePresent
			baseValue = previous.BaseValue
		} else if currentPresent {
			if target.Message == "" {
				// A healthy/no-status reconcile must not claim or rewrite an
				// operator-owned base announce.
				continue
			}
			if knownBasePresent, knownBaseValue, known := splitKnownManagedAnnounce(currentValue, target.Message, target.KnownHealthyMessage); known {
				basePresent = knownBasePresent
				baseValue = knownBaseValue
			} else if !isAppendableBaseAnnounce(currentValue) {
				conflicts = append(conflicts, fmt.Sprintf("%s: existing announce is neither an appendable single-line rwEncodeBase64 value nor a recognized managed suffix and was left untouched", squad.Name))
				continue
			} else {
				basePresent = true
				baseValue = currentValue
			}
		}

		targetValue := composeManagedAnnounce(basePresent, baseValue, target.Message)
		if target.Message == "" && !wasManaged {
			continue
		}
		if owned && target.Message != "" && currentValue == targetValue {
			if previous.Message != target.Message ||
				!stringMapEqual(previous.Groups, target.Groups) ||
				!stringMapEqual(previous.PartialGroups, target.PartialGroups) ||
				!stringMapEqual(previous.MaintenanceGroups, target.MaintenanceGroups) {
				previous.Message = target.Message
				previous.Groups = cloneStringMap(target.Groups)
				previous.PartialGroups = cloneStringMap(target.PartialGroups)
				previous.MaintenanceGroups = cloneStringMap(target.MaintenanceGroups)
				previous.UpdatedAt = now
				runtime.Managed[externalUUID] = previous
				runtimeChanged = true
			}
			continue
		}

		headers := withoutHeader(squad.ResponseHeadersAdd, announceHeader)
		if target.Message != "" {
			headers[announceHeader] = targetValue
		} else if basePresent {
			headers[announceHeader] = baseValue
		}
		ctx, cancel = s.requestContext(parent)
		updateErr := s.api.UpdateExternalHeaders(ctx, externalUUID, headers)
		cancel()
		if updateErr != nil {
			errorsSeen = append(errorsSeen, fmt.Sprintf("update %s: %v", squad.Name, updateErr))
			continue
		}
		squad.ResponseHeadersAdd = headers
		externalByUUID[externalUUID] = squad
		if target.Message == "" {
			delete(runtime.Managed, externalUUID)
		} else {
			runtime.Managed[externalUUID] = ManagedAnnouncement{
				Value:             targetValue,
				Message:           target.Message,
				BasePresent:       basePresent,
				BaseValue:         baseValue,
				Groups:            cloneStringMap(target.Groups),
				PartialGroups:     cloneStringMap(target.PartialGroups),
				MaintenanceGroups: cloneStringMap(target.MaintenanceGroups),
				UpdatedAt:         now,
			}
		}
		runtimeChanged = true
	}

	if runtimeChanged {
		if err := writeRuntimeFile(s.runtimePath, runtime, now); err != nil {
			s.mu.Lock()
			s.runtime = runtime
			s.recoverySince = nextRecovery
			s.mu.Unlock()
			return fmt.Errorf("persist Remnawave announce ownership after remote update: %w", err)
		}
		runtime.UpdatedAt = now
	}
	updatedExternal := make([]ExternalSquad, 0, len(externalByUUID))
	for _, squad := range externalByUUID {
		updatedExternal = append(updatedExternal, squad)
	}
	sort.Slice(updatedExternal, func(i, j int) bool {
		return strings.ToLower(updatedExternal[i].Name) < strings.ToLower(updatedExternal[j].Name)
	})

	status := ReconcileStatus{
		LastTopologyAt:  topology.LoadedAt,
		LastReconcileAt: now,
		Conflicts:       conflicts,
		Announcements:   announcementStatuses(updatedExternal, runtime.Managed),
	}
	if len(errorsSeen) > 0 {
		status.LastError = strings.Join(errorsSeen, "; ")
	}
	s.mu.Lock()
	s.runtime = runtime
	s.recoverySince = nextRecovery
	s.topology.ExternalSquads = cloneExternalSquads(updatedExternal)
	if !s.topology.LoadedAt.IsZero() {
		status.LastTopologyAt = s.topology.LoadedAt
	}
	s.status = status
	s.mu.Unlock()
	if len(errorsSeen) > 0 {
		return fmt.Errorf("%s", strings.Join(errorsSeen, "; "))
	}
	return nil
}

func (s *Service) proxySnapshot() ([]*models.ProxyConfig, map[string]checker.ProxyStatusDetails, map[string]bool) {
	if s.proxySource == nil {
		return nil, map[string]checker.ProxyStatusDetails{}, map[string]bool{}
	}
	proxies := s.proxySource.GetProxies()
	active := make([]*models.ProxyConfig, 0, len(proxies))
	statuses := make(map[string]checker.ProxyStatusDetails, len(proxies))
	maintenance := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.StableID == "" {
			continue
		}
		active = append(active, proxy)
		maintenance[proxy.StableID] = !s.proxySource.MonitoringEnabled(proxy.StableID)
		if details, err := s.proxySource.GetProxyStatusDetailsIncludingMaintenance(proxy.StableID); err == nil {
			statuses[proxy.StableID] = details
		}
	}
	return active, statuses, maintenance
}

func (s *Service) activeCheckEndpointStableIDs() map[string]bool {
	result := make(map[string]bool)
	if s.incidentSource == nil {
		return result
	}
	for _, incident := range s.incidentSource.Incidents(0) {
		if incident.Kind != "mass" || incident.Status != "active" || incident.CauseCode != checker.FailureCodeCheckEndpoint {
			continue
		}
		for _, stableID := range incident.StableIDs {
			result[stableID] = true
		}
	}
	return result
}

func computeDesiredAnnouncements(
	config ConfigFile,
	runtime RuntimeFile,
	topology Topology,
	proxies []*models.ProxyConfig,
	statuses map[string]checker.ProxyStatusDetails,
	maintenance map[string]bool,
	observations map[string]int,
	checkEndpointIDs map[string]bool,
	recoverySince map[string]time.Time,
	now time.Time,
) (map[string]desiredAnnouncement, map[string]time.Time, error) {
	desired := make(map[string]desiredAnnouncement)
	for externalUUID := range runtime.Managed {
		desired[externalUUID] = desiredAnnouncement{}
	}
	if !config.Policy.Enabled {
		return desired, map[string]time.Time{}, nil
	}
	if len(config.SquadPairs) > 0 && topology.LoadedAt.IsZero() {
		return nil, recoverySince, fmt.Errorf("Remnawave topology has not been loaded")
	}

	hostByUUID := make(map[string]Host, len(topology.Hosts))
	for _, host := range topology.Hosts {
		hostByUUID[host.UUID] = host
	}
	internalByUUID := make(map[string]InternalSquad, len(topology.InternalSquads))
	for _, squad := range topology.InternalSquads {
		internalByUUID[squad.UUID] = squad
	}
	externalByUUID := make(map[string]ExternalSquad, len(topology.ExternalSquads))
	for _, squad := range topology.ExternalSquads {
		externalByUUID[squad.UUID] = squad
	}
	proxyByStableID := make(map[string]*models.ProxyConfig, len(proxies))
	for _, proxy := range proxies {
		if proxy != nil && proxy.StableID != "" {
			proxyByStableID[proxy.StableID] = proxy
		}
	}

	nextRecovery := make(map[string]time.Time)
	for _, pair := range config.SquadPairs {
		if pair.MonitoringOnly {
			continue
		}
		internal, ok := internalByUUID[pair.InternalSquadUUID]
		if !ok {
			return nil, recoverySince, fmt.Errorf("configured internal squad %s no longer exists", pair.InternalSquadUUID)
		}
		if _, ok := externalByUUID[pair.ExternalSquadUUID]; !ok {
			return nil, recoverySince, fmt.Errorf("configured external squad %s no longer exists", pair.ExternalSquadUUID)
		}
		visibleHosts := visibleHostSet(internal, topology.Hosts)
		locations, err := effectiveLocations(config)
		if err != nil {
			return nil, recoverySince, err
		}
		groups := evaluateLocations(
			locations,
			config.Policy,
			visibleHosts,
			hostByUUID,
			proxyByStableID,
			statuses,
			maintenance,
			observations,
			checkEndpointIDs,
			now,
		)
		previous := runtime.Managed[pair.ExternalSquadUUID]
		if len(groups) == 0 {
			desired[pair.ExternalSquadUUID] = desiredAnnouncement{}
			continue
		}
		announced := make(map[string]string)
		partial := make(map[string]string)
		maintenanceGroups := make(map[string]string)
		allHealthy := true
		for key, group := range groups {
			if group.State != groupHealthy {
				allHealthy = false
			}

			previousState := ""
			if _, wasDown := previous.Groups[key]; wasDown {
				previousState = groupDown
			} else if _, wasPartial := previous.PartialGroups[key]; wasPartial {
				previousState = groupPartial
			} else if _, wasMaintenance := previous.MaintenanceGroups[key]; wasMaintenance {
				previousState = groupMaintenance
			}
			if group.State == groupDown {
				announced[key] = group.Label
				continue
			}
			if group.State == groupMaintenance {
				maintenanceGroups[key] = group.Label
				continue
			}
			if group.State == groupPartial && previousState != groupDown && previousState != groupMaintenance {
				partial[key] = group.Label
				continue
			}
			if previousState == "" {
				continue
			}

			recoveryKey := pair.ExternalSquadUUID + "\x00" + key
			switch group.State {
			case groupHealthy, groupPartial:
				since, seen := recoverySince[recoveryKey]
				if !seen {
					since = now
				}
				if now.Sub(since) < time.Duration(config.Policy.RecoveryMinutes)*time.Minute {
					if previousState == groupDown {
						announced[key] = group.Label
					} else if previousState == groupMaintenance {
						maintenanceGroups[key] = group.Label
					} else {
						partial[key] = group.Label
					}
					nextRecovery[recoveryKey] = since
				} else if group.State == groupPartial {
					partial[key] = group.Label
				}
			case groupPending, groupAmbiguous:
				if previousState == groupDown {
					announced[key] = group.Label
				} else if previousState == groupMaintenance {
					maintenanceGroups[key] = group.Label
				} else {
					partial[key] = group.Label
				}
			}
		}
		healthy, err := healthyMessage(config.Policy.Messages, len(groups))
		if err != nil {
			return nil, recoverySince, err
		}
		message := ""
		if len(announced) > 0 || len(maintenanceGroups) > 0 {
			outage := ""
			var outageErr error
			if len(announced) > 0 {
				outage, outageErr = outageMessage(config.Policy.Messages, announced, len(groups))
			}
			if outageErr != nil {
				return nil, recoverySince, outageErr
			}
			maintenanceText := ""
			var maintenanceErr error
			if len(maintenanceGroups) > 0 {
				maintenanceText, maintenanceErr = maintenanceMessage(config.Policy.Messages, maintenanceGroups, len(groups))
			}
			if maintenanceErr != nil {
				return nil, recoverySince, maintenanceErr
			}
			message, err = combineOutageAndMaintenanceMessages(config.Policy.Messages, outage, maintenanceText, len(announced), len(maintenanceGroups), len(groups))
			if err != nil {
				return nil, recoverySince, err
			}
		} else if len(partial) > 0 {
			message, err = partialAvailabilityMessage(config.Policy.Messages, partial, len(groups))
			if err != nil {
				return nil, recoverySince, err
			}
		} else if allHealthy || (len(previous.Groups) == 0 && len(previous.PartialGroups) == 0 && len(previous.MaintenanceGroups) == 0 && previous.Message == healthy) {
			message = healthy
		}
		desired[pair.ExternalSquadUUID] = desiredAnnouncement{
			Message:             message,
			KnownHealthyMessage: healthy,
			Groups:              announced,
			PartialGroups:       partial,
			MaintenanceGroups:   maintenanceGroups,
		}
	}
	return desired, nextRecovery, nil
}

func visibleHostSet(internal InternalSquad, hosts []Host) map[string]bool {
	inbounds := make(map[string]bool, len(internal.Inbounds))
	for _, inbound := range internal.Inbounds {
		if inbound.UUID != "" {
			inbounds[inbound.UUID] = true
		}
	}
	result := make(map[string]bool)
	for _, host := range hosts {
		if host.UUID == "" || host.IsDisabled || host.IsHidden || !inbounds[host.Inbound.ConfigProfileInboundUUID] {
			continue
		}
		excluded := false
		for _, squadUUID := range host.ExcludedInternalSquads {
			if squadUUID == internal.UUID {
				excluded = true
				break
			}
		}
		if !excluded {
			result[host.UUID] = true
		}
	}
	return result
}

func evaluateLocations(
	locations map[string]AnnounceLocation,
	policy Policy,
	visibleHosts map[string]bool,
	hostByUUID map[string]Host,
	proxyByStableID map[string]*models.ProxyConfig,
	statuses map[string]checker.ProxyStatusDetails,
	maintenance map[string]bool,
	observations map[string]int,
	checkEndpointIDs map[string]bool,
	now time.Time,
) map[string]groupEvaluation {
	type aggregate struct {
		label           string
		hasHealthy      bool
		hasPending      bool
		hasAmbiguous    bool
		members         int
		down            int
		maintenanceDown int
	}
	result := make(map[string]groupEvaluation, len(locations))
	for key, location := range locations {
		item := &aggregate{label: publicLocationLabel(location, key)}
		for stableID, hostUUID := range location.Members {
			if !visibleHosts[hostUUID] || proxyByStableID[stableID] == nil {
				continue
			}
			if _, ok := hostByUUID[hostUUID]; !ok {
				continue
			}
			item.members++
			if checkEndpointIDs[stableID] {
				item.hasAmbiguous = true
				continue
			}
			details, ok := statuses[stableID]
			if !ok {
				item.hasPending = true
				continue
			}
			if details.Online {
				item.hasHealthy = true
				continue
			}
			failureSince := details.ServiceFailureSince()
			if failureSince.IsZero() || now.Sub(failureSince) < time.Duration(policy.OutageMinutes)*time.Minute || observations[stableID] < policy.MinimumFailures {
				item.hasPending = true
				continue
			}
			item.down++
			if maintenance[stableID] {
				item.maintenanceDown++
			}
		}
		if item.members == 0 {
			continue
		}
		state := groupDown
		switch {
		case item.hasHealthy && item.down > 0:
			state = groupPartial
		case item.hasHealthy:
			state = groupHealthy
		case item.hasAmbiguous:
			state = groupAmbiguous
		case item.hasPending || item.down < item.members:
			state = groupPending
		case item.maintenanceDown == item.members:
			state = groupMaintenance
		}
		result[key] = groupEvaluation{Key: key, Label: item.label, State: state}
	}
	return result
}

func publicLocationLabel(location AnnounceLocation, locationKey string) string {
	if label := strings.TrimSpace(location.PublicLabel); label != "" {
		return label
	}
	for _, candidate := range []string{strings.TrimSpace(locationKey)} {
		if candidate != "" && validateDisplayText("public label", candidate, 80) == nil {
			return candidate
		}
	}
	return "Локация"
}

func canonicalSettingsLocations(settings Settings) (map[string]AnnounceLocation, error) {
	if settings.Locations == nil && settings.NodeMappings == nil {
		return nil, fmt.Errorf("locations is required")
	}
	if settings.Locations == nil {
		return locationsFromNodeMappings(settings.NodeMappings)
	}
	locations, err := normalizeLocationMap(settings.Locations)
	if err != nil {
		return nil, err
	}
	if settings.NodeMappings == nil {
		return locations, nil
	}
	legacy, err := locationsFromNodeMappings(settings.NodeMappings)
	if err != nil {
		return nil, err
	}
	if !announceLocationsEqual(locations, legacy) {
		return nil, fmt.Errorf("locations and deprecated nodeMappings describe different announce locations")
	}
	return locations, nil
}

func effectiveLocations(config ConfigFile) (map[string]AnnounceLocation, error) {
	if len(config.Locations) > 0 || len(config.NodeMappings) == 0 {
		return config.Locations, nil
	}
	return locationsFromNodeMappings(config.NodeMappings)
}

func announceLocationsEqual(left, right map[string]AnnounceLocation) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftLocation := range left {
		rightLocation, ok := right[key]
		if !ok || leftLocation.PublicLabel != rightLocation.PublicLabel || !stringMapEqual(leftLocation.Members, rightLocation.Members) {
			return false
		}
	}
	return true
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	config := s.config
	config.SquadPairs = append([]SquadPair(nil), config.SquadPairs...)
	config.Locations = cloneLocations(config.Locations)
	config.NodeMappings = cloneNodeMappings(config.NodeMappings)
	topology := cloneTopology(s.topology)
	status := s.status
	status.Conflicts = append([]string(nil), status.Conflicts...)
	status.Announcements = append([]RemoteAnnouncementStatus(nil), status.Announcements...)
	s.mu.RUnlock()
	for index := range topology.ExternalSquads {
		// Other response headers are unrelated to this UI and may contain
		// operational values. Only the derived announce status is exposed.
		topology.ExternalSquads[index].ResponseHeadersAdd = nil
	}
	return Snapshot{
		Connection: ConnectionInfo{
			Enabled:            s.masterEnabled,
			Configured:         s.api != nil,
			APIURL:             s.apiURL,
			APITokenConfigured: s.apiTokenConfigured,
		},
		Settings: configSettings(config),
		Topology: topology,
		Proxies:  s.proxyOptions(),
		Status:   status,
	}
}

func (s *Service) proxyOptions() []ProxyOption {
	if s.proxySource == nil {
		return []ProxyOption{}
	}
	proxies := s.proxySource.GetProxies()
	result := make([]ProxyOption, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.StableID == "" {
			continue
		}
		result = append(result, ProxyOption{
			StableID: proxy.StableID,
			Name:     proxy.Name,
			SubName:  proxy.SubName,
			Server:   proxy.Server,
			Port:     proxy.Port,
			Protocol: proxy.Protocol,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if strings.ToLower(result[i].Name) == strings.ToLower(result[j].Name) {
			return result[i].StableID < result[j].StableID
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func (s *Service) setLastError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.status.LastError = err.Error()
	s.mu.Unlock()
}

func cloneTopology(input Topology) Topology {
	result := Topology{
		LoadedAt:       input.LoadedAt,
		Hosts:          append([]Host(nil), input.Hosts...),
		InternalSquads: append([]InternalSquad(nil), input.InternalSquads...),
		ExternalSquads: cloneExternalSquads(input.ExternalSquads),
	}
	for index := range result.Hosts {
		result.Hosts[index].ExcludedInternalSquads = append([]string(nil), result.Hosts[index].ExcludedInternalSquads...)
	}
	for index := range result.InternalSquads {
		result.InternalSquads[index].Inbounds = append([]InternalInbound(nil), result.InternalSquads[index].Inbounds...)
	}
	return result
}

func cloneExternalSquads(input []ExternalSquad) []ExternalSquad {
	result := append([]ExternalSquad(nil), input...)
	for index := range result {
		result[index].ResponseHeadersAdd = cloneStringMap(result[index].ResponseHeadersAdd)
	}
	return result
}

func sortTopology(hosts []Host, internal []InternalSquad, external []ExternalSquad) {
	sort.Slice(hosts, func(i, j int) bool { return strings.ToLower(hosts[i].Remark) < strings.ToLower(hosts[j].Remark) })
	sort.Slice(internal, func(i, j int) bool { return strings.ToLower(internal[i].Name) < strings.ToLower(internal[j].Name) })
	sort.Slice(external, func(i, j int) bool { return strings.ToLower(external[i].Name) < strings.ToLower(external[j].Name) })
}

func headerValue(headers map[string]string, name string) (string, bool, bool) {
	var result string
	found := false
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			if found {
				return "", true, true
			}
			result = value
			found = true
		}
	}
	return result, found, false
}

func withoutHeader(headers map[string]string, name string) map[string]string {
	result := make(map[string]string, len(headers))
	for key, value := range headers {
		if !strings.EqualFold(key, name) {
			result[key] = value
		}
	}
	return result
}

func stringMapEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func announcementStatuses(external []ExternalSquad, managed map[string]ManagedAnnouncement) []RemoteAnnouncementStatus {
	result := make([]RemoteAnnouncementStatus, 0)
	seen := make(map[string]bool)
	for _, squad := range external {
		value, present, duplicateHeader := headerValue(squad.ResponseHeadersAdd, announceHeader)
		owned, isManaged := managed[squad.UUID]
		status := RemoteAnnouncementStatus{
			ExternalSquadUUID: squad.UUID,
			ExternalSquadName: squad.Name,
			Present:           present,
			Managed:           isManaged && present && !duplicateHeader && value == owned.Value,
			PreservesBase:     (!isManaged && present && !duplicateHeader && isAppendableBaseAnnounce(value)) || (isManaged && owned.BasePresent),
		}
		if status.Managed {
			status.Message = owned.Message
		}
		if present || isManaged {
			result = append(result, status)
		}
		seen[squad.UUID] = true
	}
	for externalUUID, owned := range managed {
		if seen[externalUUID] {
			continue
		}
		result = append(result, RemoteAnnouncementStatus{
			ExternalSquadUUID: externalUUID,
			Managed:           true,
			PreservesBase:     owned.BasePresent,
			Message:           owned.Message,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].ExternalSquadName) < strings.ToLower(result[j].ExternalSquadName)
	})
	return result
}
