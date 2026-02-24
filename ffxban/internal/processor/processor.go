package processor

import (
	"context"
	"errors"
	"ffxban/internal/config"
	"ffxban/internal/models"
	"ffxban/internal/services/alerter"
	"ffxban/internal/services/blocker"
	"ffxban/internal/services/panel"
	"ffxban/internal/services/storage"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogProcessor обрабатывает входящие логи.
type LogProcessor struct {
	storage           storage.IPStorage
	blocker           blocker.Blocker
	alerter           alerter.Notifier
	limitProvider     panel.UserLimitProvider
	networkClassifier networkClassifier
	cfg               *config.Config
	logChannel        chan []models.LogEntry // Канал для получения пачек логов
	sideEffectChannel chan func()            // Канал для побочных задач (алерты, очистка)
	// OnBatchProcessed вызывается после обработки каждой пачки логов.
	// Используется для уведомления WebSocket-клиентов панели об изменениях данных.
	OnBatchProcessed func()
	eventsMu          sync.RWMutex
	recentEvents      map[string][]userEvent
	lastNode          map[string]string
	lastNetworkType   map[string]string
	lastNodeSwitchAt  map[string]time.Time
	lastNodeParallel  map[string]string
	ipLastNode        map[string]map[string]string
	lastSeenAt        map[string]time.Time
	lastBlockedIPs    map[string][]string
	heuristics        map[string]models.HeuristicMetrics
	history           map[string][]models.UserEventLog
	activeBans        map[string]activeBanState
	nodeState         map[string]models.NodeRuntimeState
	sharingState      map[string]*sharingState
	historyLimit      int
}

type userEvent struct {
	SourceIP string
	NodeName string
	SeenAt   time.Time
}

type activeBanState struct {
	NodeName  string
	Duration  string
	SetAt     time.Time
	ExpiresAt time.Time
}

type sharingState struct {
	Triggers        []time.Time
	ExceedSince     time.Time
	ViolatorSince   time.Time
	BanlistSince    time.Time
	PermanentSince  time.Time
	PermanentPend   bool
	LastTriggerAt   time.Time
	LastSourceCount int
	ViolationIPs    map[string]struct{}
	ViolationNodes  map[string]struct{}
	BanCount        int
	BanCountLoaded  bool // загружен ли BanCount из Redis
	CountrySeen     map[string]struct{}
	GeoMismatchAt   time.Time
}

type networkSummary struct {
	DominantType string
	Counts       map[string]int
	Total        int
}

type networkClassifier interface {
	Classify(ctx context.Context, ip string) models.NetworkInfo
}

// NewLogProcessor создает новый экземпляр LogProcessor.
func NewLogProcessor(
	s storage.IPStorage,
	b blocker.Blocker,
	a alerter.Notifier,
	limitProvider panel.UserLimitProvider,
	networkClassifier networkClassifier,
	cfg *config.Config,
) *LogProcessor {
	return &LogProcessor{
		storage:           s,
		blocker:           b,
		alerter:           a,
		limitProvider:     limitProvider,
		networkClassifier: networkClassifier,
		cfg:               cfg,
		logChannel:        make(chan []models.LogEntry, cfg.LogChannelBufferSize),
		sideEffectChannel: make(chan func(), cfg.SideEffectChannelBufferSize),
		recentEvents:      make(map[string][]userEvent),
		lastNode:          make(map[string]string),
		lastNetworkType:   make(map[string]string),
		lastNodeSwitchAt:  make(map[string]time.Time),
		lastNodeParallel:  make(map[string]string),
		ipLastNode:        make(map[string]map[string]string),
		lastSeenAt:        make(map[string]time.Time),
		lastBlockedIPs:    make(map[string][]string),
		heuristics:        make(map[string]models.HeuristicMetrics),
		history:           make(map[string][]models.UserEventLog),
		activeBans:        make(map[string]activeBanState),
		nodeState:         make(map[string]models.NodeRuntimeState),
		sharingState:      make(map[string]*sharingState),
		historyLimit:      500,
	}
}

// StartWorkerPool запускает пул горутин-воркеров для обработки логов.
func (p *LogProcessor) StartWorkerPool(ctx context.Context, mainWg *sync.WaitGroup) {
	defer mainWg.Done()

	var workerWg sync.WaitGroup
	log.Printf("Запуск пула воркеров обработки логов в количестве %d...", p.cfg.WorkerPoolSize)

	for i := 0; i < p.cfg.WorkerPoolSize; i++ {
		workerWg.Add(1)
		go func(workerID int) {
			defer workerWg.Done()
			log.Printf("Воркер обработки логов %d запущен", workerID)
			for entries := range p.logChannel {
				p.ProcessEntries(ctx, entries)
				if p.OnBatchProcessed != nil {
					p.OnBatchProcessed()
				}
			}
			log.Printf("Воркер обработки логов %d останавливается.", workerID)
		}(i + 1)
	}

	<-ctx.Done()
	log.Println("Получен сигнал остановки для воркеров обработки логов. Закрываю канал...")
	close(p.logChannel)
	workerWg.Wait()
	log.Println("Все воркеры обработки логов успешно остановлены.")
}

// StartSideEffectWorkerPool запускает пул воркеров для выполнения побочных задач.
func (p *LogProcessor) StartSideEffectWorkerPool(ctx context.Context, mainWg *sync.WaitGroup) {
	defer mainWg.Done()

	var workerWg sync.WaitGroup
	log.Printf("Запуск пула воркеров побочных задач в количестве %d...", p.cfg.SideEffectWorkerPoolSize)

	for i := 0; i < p.cfg.SideEffectWorkerPoolSize; i++ {
		workerWg.Add(1)
		go func(workerID int) {
			defer workerWg.Done()
			log.Printf("Воркер побочных задач %d запущен", workerID)
			for task := range p.sideEffectChannel {
				// Проверяем, не был ли контекст отменен перед выполнением задачи
				select {
				case <-ctx.Done():
					log.Printf("Воркер побочных задач %d пропустил задачу из-за отмены контекста.", workerID)
				default:
					task()
				}
			}
			log.Printf("Воркер побочных задач %d останавливается.", workerID)
		}(i + 1)
	}

	<-ctx.Done()
	log.Println("Получен сигнал остановки для воркеров побочных задач. Закрываю канал...")
	close(p.sideEffectChannel)
	workerWg.Wait()
	log.Println("Все воркеры побочных задач успешно остановлены.")
}

// EnqueueEntries добавляет пачку логов в очередь на обработку.
func (p *LogProcessor) EnqueueEntries(entries []models.LogEntry) error {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Попытка записи в закрытый канал логов. Сервис находится в процессе остановки.")
		}
	}()

	select {
	case p.logChannel <- entries:
		return nil
	default:
		return errors.New("log channel is full, rejecting new entries")
	}
}

// enqueueSideEffectTask добавляет побочную задачу в очередь на выполнение.
func (p *LogProcessor) enqueueSideEffectTask(task func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("Попытка записи в закрытый канал побочных задач. Сервис находится в процессе остановки.")
		}
	}()

	select {
	case p.sideEffectChannel <- task:
		// Задача успешно добавлена в очередь
	default:
		log.Println("Warning: очередь побочных задач заполнена. Задача отброшена.")
	}
}

// ProcessEntries обрабатывает пачку записей логов.
func (p *LogProcessor) ProcessEntries(ctx context.Context, entries []models.LogEntry) {
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			log.Printf("Обработка пачки прервана из-за отмены контекста: %v", ctx.Err())
			return
		default:
			p.processSingleEntry(ctx, entry)
		}
	}
}

func (p *LogProcessor) processSingleEntry(ctx context.Context, entry models.LogEntry) {
	prevNode, currentNode := p.recordUserEvent(entry)
	networkInfo := p.classifyIP(ctx, entry.SourceIP)
	now := time.Now()
	if err := p.storage.SetIPNode(ctx, entry.UserEmail, entry.SourceIP, entry.NodeName, p.cfg.UserIPTTL); err != nil {
		log.Printf("Не удалось сохранить привязку IP->нода для %s (%s): %v", entry.UserEmail, entry.SourceIP, err)
	}

	if p.cfg.ExcludedUsers[entry.UserEmail] {
		return // Пользователь в списке исключений
	}

	if p.shouldLogNodeSwitch(entry.UserEmail, prevNode, currentNode, now) {
		p.addHistory(entry.UserEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "node_switch",
			Message:   fmt.Sprintf("Смена ноды: %s -> %s", prevNode, currentNode),
			SourceIP:  entry.SourceIP,
			NodeName:  currentNode,
		})
	}

	userIPLimit := p.getUserIPLimit(ctx, entry.UserEmail)
	debugMarker := p.getDebugMarker(entry.UserEmail)

	res, err := p.storage.CheckAndAddIP(ctx, entry.UserEmail, entry.SourceIP, userIPLimit, p.cfg.UserIPTTL, p.cfg.AlertCooldown)
	if err != nil {
		p.addHistory(entry.UserEmail, models.UserEventLog{
			Timestamp: time.Now(),
			Type:      "process_error",
			Message:   err.Error(),
			SourceIP:  entry.SourceIP,
			NodeName:  entry.NodeName,
		})
		// Проверяем, не была ли ошибка вызвана отменой контекста
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Операция CheckAndAddIP отменена для %s: %v", entry.UserEmail, err)
		} else {
			log.Printf("Ошибка обработки записи для %s: %v", entry.UserEmail, err)
		}
		return
	}

	activeIPs := make([]string, 0, len(res.AllUserIPs))
	if len(res.AllUserIPs) > 0 {
		activeIPs = append(activeIPs, res.AllUserIPs...)
	} else {
		activeMap, activeErr := p.storage.GetUserActiveIPs(ctx, entry.UserEmail)
		if activeErr != nil {
			log.Printf("Не удалось получить active IP пользователя %s: %v", entry.UserEmail, activeErr)
		} else {
			activeIPs = make([]string, 0, len(activeMap))
			for ip := range activeMap {
				ip = strings.TrimSpace(ip)
				if ip != "" {
					activeIPs = append(activeIPs, ip)
				}
			}
		}
	}

	sourceCount := p.countEffectiveSharingSources(ctx, entry.UserEmail, activeIPs, entry.SourceIP, now)
	if sourceCount == 0 && res.CurrentIPCount > 0 {
		sourceCount = int(res.CurrentIPCount)
	}

	p.updateSharingStatus(entry.UserEmail, entry.SourceIP, entry.NodeName, networkInfo.Country, userIPLimit, int(res.CurrentIPCount), activeIPs, sourceCount, now)
	p.trackNodeParallel(ctx, entry.UserEmail, entry.SourceIP, entry.NodeName, now)

	if res.StatusCode == 0 && res.IsNewIP {
		newIPMessage := "Зафиксирован новый IP"
		if networkInfo.Type != "" && networkInfo.Type != "unknown" {
			newIPMessage = "Зафиксирован новый IP (" + networkInfo.Type + ")"
		}
		p.addHistory(entry.UserEmail, models.UserEventLog{
			Timestamp: time.Now(),
			Type:      "new_ip",
			Message:   newIPMessage,
			SourceIP:  entry.SourceIP,
			NodeName:  entry.NodeName,
		})
		log.Printf("Новый IP для пользователя %s%s: %s. Всего IP: %d/%d",
			entry.UserEmail, debugMarker, entry.SourceIP, res.CurrentIPCount, userIPLimit)

		if changed, prevType, currType := p.updateLastNetworkType(entry.UserEmail, normalizeNetworkTypeLocal(networkInfo.Type)); changed {
			p.addHistory(entry.UserEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "network_type_changed",
				Message:   fmt.Sprintf("Тип сети изменился: %s -> %s", prevType, currType),
				SourceIP:  entry.SourceIP,
				NodeName:  entry.NodeName,
			})
		}
	}

	if res.StatusCode == 1 { // Лимит превышен, нужна блокировка
		p.addHistory(entry.UserEmail, models.UserEventLog{
			Timestamp: time.Now(),
			Type:      "limit_exceeded",
			Message:   fmt.Sprintf("Превышение лимита IP: ip=%d, sources=%d/%d", res.CurrentIPCount, sourceCount, userIPLimit),
			SourceIP:  entry.SourceIP,
			NodeName:  entry.NodeName,
		})
		log.Printf("ПРЕВЫШЕНИЕ ЛИМИТА%s: Пользователь %s, IP-адресов: %d/%d",
			debugMarker, entry.UserEmail, res.CurrentIPCount, userIPLimit)

		// Упрощенный критерий: блокируем только когда превышение подтверждается
		// количеством независимых источников (/24 при SUBNET_GROUPING=true).
		if sourceCount <= userIPLimit {
			msg := fmt.Sprintf("Превышение по IP игнорировано: sources=%d <= limit=%d", sourceCount, userIPLimit)
			p.addHistory(entry.UserEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "limit_exceeded_ignored",
				Message:   msg,
				SourceIP:  entry.SourceIP,
				NodeName:  entry.NodeName,
			})
			log.Printf("Блокировка для %s пропущена: sources=%d <= limit=%d", entry.UserEmail, sourceCount, userIPLimit)
			return
		}
		sharingStatus, hasSharingStatus := p.GetSharingStatus(entry.UserEmail)
		ipsToBlock := p.filterExcludedIPs(activeIPs, entry.UserEmail)
		if p.cfg.SharingDetectionEnabled && p.cfg.SharingBlockOnBanlistOnly {
			if !hasSharingStatus || !sharingStatus.InBanlist {
				triggerHits := 0
				triggerRequired := p.sharingTriggerCount()
				if hasSharingStatus {
					triggerHits = sharingStatus.TriggerHits
					triggerRequired = sharingStatus.TriggerRequired
				}
				msg := fmt.Sprintf(
					"Ожидание ban-list: sources=%d/%d, triggers=%d/%d",
					sourceCount,
					userIPLimit,
					triggerHits,
					triggerRequired,
				)
				p.addHistory(entry.UserEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "sharing_pending_banlist",
					Message:   msg,
					SourceIP:  entry.SourceIP,
					NodeName:  entry.NodeName,
				})
				log.Printf(
					"Блокировка для %s отложена (sharing_pending_banlist): sources=%d/%d triggers=%d/%d",
					entry.UserEmail,
					sourceCount,
					userIPLimit,
					triggerHits,
					triggerRequired,
				)
				return
			}
		} else if p.cfg.SharingDetectionEnabled && p.cfg.SharingBlockOnViolatorOnly {
			if !hasSharingStatus || !sharingStatus.Violator {
				triggerHits := 0
				triggerRequired := p.sharingTriggerCount()
				if hasSharingStatus {
					triggerHits = sharingStatus.TriggerHits
					triggerRequired = sharingStatus.TriggerRequired
				}
				msg := fmt.Sprintf(
					"Ожидание подтверждения шаринга: sources=%d/%d, triggers=%d/%d",
					sourceCount,
					userIPLimit,
					triggerHits,
					triggerRequired,
				)
				p.addHistory(entry.UserEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "sharing_pending",
					Message:   msg,
					SourceIP:  entry.SourceIP,
					NodeName:  entry.NodeName,
				})
				log.Printf(
					"Блокировка для %s отложена (sharing_pending): sources=%d/%d triggers=%d/%d",
					entry.UserEmail,
					sourceCount,
					userIPLimit,
					triggerHits,
					triggerRequired,
				)
				return
			}
		}

		if len(ipsToBlock) == 0 {
			p.addHistory(entry.UserEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "block_skipped",
				Message:   "Блокировка пропущена: все IP в исключениях",
				SourceIP:  entry.SourceIP,
				NodeName:  entry.NodeName,
			})
			return
		}

		netSummary := p.buildNetworkSummary(ctx, ipsToBlock)
		if len(ipsToBlock) > 0 {
			var blockErr error
			// Block IPs (Remnawave/RabbitMQ)
			if err := p.blocker.BlockIPs(ipsToBlock, p.cfg.BlockDuration); err != nil {
				blockErr = err
				log.Printf("Ошибка отправки сообщения о блокировке IP: %v", err)
			}
			// Block User (3x-ui)
			if err := p.blocker.BlockUser(entry.UserEmail, p.cfg.BlockDuration); err != nil {
				if blockErr == nil {
					blockErr = err
				} else {
					blockErr = fmt.Errorf("%v; %v", blockErr, err)
				}
				log.Printf("Ошибка блокировки пользователя %s: %v", entry.UserEmail, err)
			}

			if blockErr != nil {
				p.addHistory(entry.UserEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "block_failed",
					Message:   "Ошибка блокировки: " + blockErr.Error(),
					SourceIP:  entry.SourceIP,
					NodeName:  entry.NodeName,
				})
			} else {
				p.RecordBlockedIPs(entry.UserEmail, ipsToBlock)
				p.addHistory(entry.UserEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "block_published",
					Message:   "Блокировка применена",
					SourceIP:  entry.SourceIP,
					NodeName:  entry.NodeName,
				})
				log.Printf("Блокировка для %s%s применена (IP: %d)", entry.UserEmail, debugMarker, len(ipsToBlock))
				blockEmail := entry.UserEmail
				p.enqueueSideEffectTask(func() {
					p.scheduleIPsClear(ctx, blockEmail)
				})
				p.enqueueSideEffectTask(func() {
					sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					p.storage.IncrUserBlockCount(sCtx, blockEmail)
				})
			}
		}

		alertPayload := models.AlertPayload{
			UserIdentifier:   entry.UserEmail,
			Username:         p.resolveAlertUsername(entry.UserEmail),
			DetectedIPsCount: len(activeIPs),
			Limit:            userIPLimit,
			AllUserIPs:       activeIPs,
			BlockDuration:    p.cfg.BlockDuration,
			ViolationType:    "sharing_suspected",
			NodeName:         entry.NodeName,
			NetworkType:      netSummary.DominantType,
		}
		p.enqueueSideEffectTask(func() {
			if err := p.alerter.SendAlert(alertPayload); err != nil {
				p.addHistory(entry.UserEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "alert_failed",
					Message:   "Ошибка webhook: " + err.Error(),
					SourceIP:  entry.SourceIP,
					NodeName:  entry.NodeName,
				})
				log.Printf("Ошибка отправки вебхук-уведомления: %v", err)
				return
			}
			p.addHistory(entry.UserEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "alert_sent",
				Message:   "Webhook-уведомление отправлено",
				SourceIP:  entry.SourceIP,
				NodeName:  entry.NodeName,
			})
		})
	}
}

func (p *LogProcessor) getUserIPLimit(_ context.Context, userEmail string) int {
	if p.cfg.DebugEmail != "" && userEmail == p.cfg.DebugEmail {
		return p.cfg.DebugIPLimit
	}

	if p.limitProvider != nil {
		if limit, ok := p.limitProvider.GetUserLimit(userEmail); ok && limit > 0 {
			return limit
		}
	}

	return p.cfg.MaxIPsPerUser
}

func (p *LogProcessor) hwidGuardRejectsBlock(userEmail string, limit int, currentCount int, concurrentExceeds bool) (bool, int, string) {
	if !p.cfg.SharingHardwareGuardEnabled || limit <= 0 || p.limitProvider == nil {
		return false, 0, "disabled_or_no_profile"
	}
	profile, ok := p.limitProvider.GetUserProfile(userEmail)
	if !ok {
		return false, 0, "profile_not_found"
	}
	hwidCount := len(profile.HWIDDevices)
	if hwidCount <= 0 {
		return false, 0, "no_hwid_devices"
	}

	forceExcess := p.cfg.NetworkForceBlockExcessIPs
	if forceExcess < 1 {
		forceExcess = 1
	}
	forceThreshold := limit + forceExcess
	if currentCount >= forceThreshold {
		return false, hwidCount, "force_threshold"
	}
	if concurrentExceeds {
		return false, hwidCount, "concurrent_exceeds"
	}
	if hwidCount <= limit {
		return true, hwidCount, "hwid_within_limit"
	}
	return false, hwidCount, "hwid_over_limit"
}

func (p *LogProcessor) resolveAlertUsername(userEmail string) string {
	alertUsername := strings.TrimSpace(userEmail)
	if p.limitProvider != nil {
		if profile, ok := p.limitProvider.GetUserProfile(userEmail); ok {
			if uname := strings.TrimSpace(profile.Username); uname != "" {
				alertUsername = uname
			}
		}
	}
	return alertUsername
}

// ResolveUserIPLimit возвращает лимит устройств пользователя с учетом debug-режима и панели.
func (p *LogProcessor) ResolveUserIPLimit(ctx context.Context, userEmail string) int {
	return p.getUserIPLimit(ctx, userEmail)
}

func (p *LogProcessor) getDebugMarker(userEmail string) string {
	if p.cfg.DebugEmail != "" && userEmail == p.cfg.DebugEmail {
		return " [DEBUG]"
	}
	return ""
}

func (p *LogProcessor) recordUserEvent(entry models.LogEntry) (string, string) {
	userKey := normalizeUserID(entry.UserEmail)
	if userKey == "" {
		return "", ""
	}

	now := time.Now()
	event := userEvent{
		SourceIP: strings.TrimSpace(entry.SourceIP),
		NodeName: strings.TrimSpace(entry.NodeName),
		SeenAt:   now,
	}

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	prevNode := strings.TrimSpace(p.lastNode[userKey])
	events := append(p.recentEvents[userKey], event)
	p.recentEvents[userKey] = p.pruneEventsLocked(events, now)
	p.lastSeenAt[userKey] = now
	if event.NodeName != "" {
		p.lastNode[userKey] = event.NodeName
		st := p.nodeState[event.NodeName]
		st.LastTrafficAt = now
		p.nodeState[event.NodeName] = st
	}
	if event.SourceIP != "" && event.NodeName != "" {
		if p.ipLastNode[userKey] == nil {
			p.ipLastNode[userKey] = make(map[string]string)
		}
		p.ipLastNode[userKey][event.SourceIP] = event.NodeName
	}

	return prevNode, event.NodeName
}

func (p *LogProcessor) shouldLogNodeSwitch(userEmail, prevNode, currNode string, now time.Time) bool {
	prevNode = strings.TrimSpace(prevNode)
	currNode = strings.TrimSpace(currNode)
	if prevNode == "" || currNode == "" || prevNode == currNode {
		return false
	}

	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return false
	}

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	lastAt := p.lastNodeSwitchAt[userKey]
	if !lastAt.IsZero() && now.Sub(lastAt) < 15*time.Second {
		return false
	}
	p.lastNodeSwitchAt[userKey] = now
	return true
}

func (p *LogProcessor) updateLastNetworkType(userEmail, currentType string) (bool, string, string) {
	currentType = normalizeNetworkTypeLocal(currentType)
	if currentType == "unknown" {
		return false, "", currentType
	}

	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return false, "", currentType
	}

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	prevType := normalizeNetworkTypeLocal(p.lastNetworkType[userKey])
	p.lastNetworkType[userKey] = currentType
	if prevType == "" || prevType == "unknown" || prevType == currentType {
		return false, prevType, currentType
	}
	return true, prevType, currentType
}

// GetUserIPNodes возвращает последнюю известную ноду для IP пользователя.
func (p *LogProcessor) GetUserIPNodes(userEmail string) map[string]string {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return nil
	}

	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	src := p.ipLastNode[userKey]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for ip, node := range src {
		out[ip] = node
	}
	return out
}

func (p *LogProcessor) trackNodeParallel(ctx context.Context, userEmail, sourceIP, nodeName string, now time.Time) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return
	}

	activeIPs, err := p.storage.GetUserActiveIPs(ctx, userEmail)
	if err != nil || len(activeIPs) == 0 {
		p.eventsMu.Lock()
		delete(p.lastNodeParallel, userKey)
		p.eventsMu.Unlock()
		return
	}

	ips := make([]string, 0, len(activeIPs))
	for ip := range activeIPs {
		ips = append(ips, ip)
	}

	storedNodes := make(map[string]string)
	if byIP, getErr := p.storage.GetUserIPNodes(ctx, userEmail, ips); getErr == nil {
		storedNodes = byIP
	}
	runtimeNodes := p.GetUserIPNodes(userEmail)

	nodeSet := make(map[string]struct{})
	sourceIP = strings.TrimSpace(sourceIP)
	nodeName = strings.TrimSpace(nodeName)
	for _, ip := range ips {
		node := strings.TrimSpace(storedNodes[ip])
		if node == "" {
			node = strings.TrimSpace(runtimeNodes[ip])
		}
		if node == "" && sourceIP != "" && ip == sourceIP {
			node = nodeName
		}
		if node == "" {
			continue
		}
		nodeSet[node] = struct{}{}
	}

	nodes := make([]string, 0, len(nodeSet))
	for node := range nodeSet {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	signature := ""
	if len(nodes) >= 2 {
		signature = strings.Join(nodes, ",")
	}

	p.eventsMu.Lock()
	prev := p.lastNodeParallel[userKey]
	if signature == "" {
		delete(p.lastNodeParallel, userKey)
		p.eventsMu.Unlock()
		return
	}
	if prev == signature {
		p.eventsMu.Unlock()
		return
	}
	p.lastNodeParallel[userKey] = signature
	p.eventsMu.Unlock()

	p.addHistory(userEmail, models.UserEventLog{
		Timestamp: now,
		Type:      "node_parallel",
		Message:   fmt.Sprintf("Одновременная активность на нодах: %s", strings.Join(nodes, ", ")),
		SourceIP:  sourceIP,
		NodeName:  nodeName,
	})
}

func (p *LogProcessor) addHistory(userEmail string, event models.UserEventLog) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	history := append(p.history[userKey], event)
	if len(history) > p.historyLimit {
		history = history[len(history)-p.historyLimit:]
	}
	p.history[userKey] = history
}

func (p *LogProcessor) updateSharingStatus(
	userEmail,
	sourceIP,
	nodeName,
	sourceCountry string,
	limit int,
	activeIPCount int,
	activeIPs []string,
	sourceCount int,
	now time.Time,
) {
	if !p.cfg.SharingDetectionEnabled {
		return
	}
	if p.cfg.ExcludedUsers[userEmail] || p.cfg.ExcludedUsers[normalizeUserID(userEmail)] {
		return
	}
	if limit <= 0 {
		return
	}

	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return
	}

	triggerCount := p.sharingTriggerCount()
	triggerPeriod := p.sharingTriggerPeriod()
	triggerMinInterval := p.sharingTriggerMinInterval()
	banlistThreshold := p.sharingBanlistThreshold()
	var logStarted bool
	var logCleared bool
	var logBanAdded bool
	var publishPermanent bool
	var permanentSeedIPs []string
	var sendPermanentAlert bool
	var permanentAlertPayload models.AlertPayload
	var clearDuration time.Duration
	var clearHits int
	var preResetHits int
	var banCount int
	var logGeoMismatch bool
	var geoMismatchCountries []string
	var actualDuration string

	p.eventsMu.Lock()
	state := p.sharingState[userKey]
	if state == nil {
		state = &sharingState{}
		p.sharingState[userKey] = state
		// Загрузить персистентный BanCount из Redis при первой инициализации
		go p.loadPersistentBanCount(userEmail, userKey)
	}

	if sourceCount == 0 && activeIPCount > 0 {
		sourceCount = activeIPCount
	}
	state.LastSourceCount = sourceCount

	p.pruneSharingTriggersLocked(state, now, triggerPeriod)
	exceedsNow := sourceCount > limit
	if exceedsNow {
		if state.ExceedSince.IsZero() {
			state.ExceedSince = now
		}
		if now.Sub(state.ExceedSince) >= p.sharingSustainDuration() {
			shouldAppendTrigger := state.LastTriggerAt.IsZero() || now.Sub(state.LastTriggerAt) >= triggerMinInterval
			if shouldAppendTrigger {
				state.Triggers = append(state.Triggers, now)
				state.LastTriggerAt = now
				p.pruneSharingTriggersLocked(state, now, triggerPeriod)
			}
		}
	} else {
		state.ExceedSince = time.Time{}
		if len(state.Triggers) > 0 {
			preResetHits = len(state.Triggers)
			state.Triggers = nil
			state.LastTriggerAt = time.Time{}
		}
	}

	if len(state.Triggers) >= triggerCount {
		if state.ViolatorSince.IsZero() {
			state.ViolatorSince = now
			logStarted = true
		}
		if state.ViolationIPs == nil {
			state.ViolationIPs = make(map[string]struct{})
		}
		if state.ViolationNodes == nil {
			state.ViolationNodes = make(map[string]struct{})
		}
		for _, ip := range activeIPs {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				state.ViolationIPs[ip] = struct{}{}
			}
		}
		if ip := strings.TrimSpace(sourceIP); ip != "" {
			state.ViolationIPs[ip] = struct{}{}
		}
		if node := strings.TrimSpace(nodeName); node != "" {
			state.ViolationNodes[node] = struct{}{}
		}
		if state.BanlistSince.IsZero() && !state.ViolatorSince.IsZero() && now.Sub(state.ViolatorSince) >= banlistThreshold {
			state.BanlistSince = now
			logBanAdded = true
			state.BanCount++
			banCount = state.BanCount
			banEmail := userEmail
			p.enqueueSideEffectTask(func() {
				sCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if newCount, err := p.storage.IncrUserBanCount(sCtx, banEmail); err == nil {
					p.eventsMu.Lock()
					if st := p.sharingState[normalizeUserID(banEmail)]; st != nil {
						st.BanCount = newCount
					}
					p.eventsMu.Unlock()
				}
			})
			if p.cfg.SharingPermanentBanEnabled && state.PermanentSince.IsZero() && !state.PermanentPend {
				state.PermanentPend = true
				publishPermanent = true
				permanentSeedIPs = make([]string, 0, len(state.ViolationIPs)+1)
				for ip := range state.ViolationIPs {
					ip = strings.TrimSpace(ip)
					if ip != "" {
						permanentSeedIPs = append(permanentSeedIPs, ip)
					}
				}
				if sourceIP = strings.TrimSpace(sourceIP); sourceIP != "" {
					permanentSeedIPs = append(permanentSeedIPs, sourceIP)
				}
			}
		}
	} else if !state.ViolatorSince.IsZero() {
		clearDuration = now.Sub(state.ViolatorSince)
		clearHits = len(state.Triggers)
		if preResetHits > 0 {
			clearHits = preResetHits
		}
		logCleared = true
		state.ViolatorSince = time.Time{}
		if state.PermanentSince.IsZero() {
			state.BanlistSince = time.Time{}
			state.ViolationIPs = nil
			state.ViolationNodes = nil
		}
	}

	// Geo-sharing tracking
	if p.cfg.GeoSharingEnabled {
		country := strings.ToLower(strings.TrimSpace(sourceCountry))
		if country != "" {
			if state.CountrySeen == nil {
				state.CountrySeen = make(map[string]struct{})
			}
			state.CountrySeen[country] = struct{}{}
		}
		minCountries := p.cfg.GeoSharingMinCountries
		if minCountries <= 0 {
			minCountries = 2
		}
		if len(state.CountrySeen) >= minCountries && state.GeoMismatchAt.IsZero() {
			state.GeoMismatchAt = now
			logGeoMismatch = true
			geoMismatchCountries = make([]string, 0, len(state.CountrySeen))
			for c := range state.CountrySeen {
				geoMismatchCountries = append(geoMismatchCountries, c)
			}
		}
	}

	p.eventsMu.Unlock()

	if logStarted {
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "violator_started",
			Message:   fmt.Sprintf("Пользователь переведен в статус нарушителя (%d/%d триггеров, sources=%d/%d)", triggerCount, triggerCount, sourceCount, limit),
			SourceIP:  sourceIP,
			NodeName:  nodeName,
		})
	}
	if logBanAdded {
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "banlist_added",
			Message:   fmt.Sprintf("Пользователь добавлен в ban-list после %.0fс непрерывного нарушения", banlistThreshold.Seconds()),
			SourceIP:  sourceIP,
			NodeName:  nodeName,
		})
	}
	if publishPermanent {
		ipSet := make(map[string]struct{}, len(permanentSeedIPs)+4)
		for _, ip := range permanentSeedIPs {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				ipSet[ip] = struct{}{}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		activeIPsMap, err := p.storage.GetUserActiveIPs(ctx, userEmail)
		cancel()
		if err != nil {
			log.Printf("Не удалось загрузить active IP для permanent-блокировки %s: %v", userEmail, err)
		}
		for ip := range activeIPsMap {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				ipSet[ip] = struct{}{}
			}
		}
		ipsToBlock := make([]string, 0, len(ipSet))
		for ip := range ipSet {
			ipsToBlock = append(ipsToBlock, ip)
		}
		sort.Strings(ipsToBlock)
		ipsToBlock = p.filterExcludedIPs(ipsToBlock, userEmail)

		permanentApplied := false
		permanentDuration := p.getEscalatedDuration(banCount)
		actualDuration = permanentDuration

		if len(ipsToBlock) == 0 {
			p.addHistory(userEmail, models.UserEventLog{
				Timestamp: now,
				Type:      "permanent_ban_skipped",
				Message:   "Перманентный бан пропущен: все IP пользователя находятся в исключениях",
				SourceIP:  sourceIP,
				NodeName:  nodeName,
			})
		} else {
			var permBlockErr error
			if err := p.blocker.BlockIPs(ipsToBlock, permanentDuration); err != nil {
				permBlockErr = err
				log.Printf("Ошибка публикации перманентной блокировки IP для %s: %v", userEmail, err)
			}
			if err := p.blocker.BlockUser(userEmail, permanentDuration); err != nil {
				if permBlockErr == nil {
					permBlockErr = err
				} else {
					permBlockErr = fmt.Errorf("%v; %v", permBlockErr, err)
				}
				log.Printf("Ошибка перманентной блокировки пользователя %s: %v", userEmail, err)
			}

			if permBlockErr != nil {
				p.addHistory(userEmail, models.UserEventLog{
					Timestamp: now,
					Type:      "permanent_ban_failed",
					Message:   "Не удалось отправить перманентную блокировку: " + permBlockErr.Error(),
					SourceIP:  sourceIP,
					NodeName:  nodeName,
				})
			} else {
				permanentApplied = true
				p.RecordBlockedIPs(userEmail, ipsToBlock)
				p.addHistory(userEmail, models.UserEventLog{
					Timestamp: now,
					Type:      "permanent_ban_applied",
					Message:   fmt.Sprintf("Пользователь заблокирован навсегда (IP: %d)", len(ipsToBlock)),
					SourceIP:  sourceIP,
					NodeName:  nodeName,
				})
				permanentSummary := p.buildNetworkSummary(context.Background(), ipsToBlock)
				permanentAlertPayload = models.AlertPayload{
					UserIdentifier:   userEmail,
					Username:         p.resolveAlertUsername(userEmail),
					DetectedIPsCount: len(ipsToBlock),
					Limit:            p.getUserIPLimit(context.Background(), userEmail),
					AllUserIPs:       ipsToBlock,
					BlockDuration:    permanentDuration,
					ViolationType:    "sharing_permanent_ban",
					NodeName:         nodeName,
					NetworkType:      permanentSummary.DominantType,
				}
				sendPermanentAlert = true
			}
		}

		p.eventsMu.Lock()
		if state := p.sharingState[userKey]; state != nil {
			state.PermanentPend = false
			if permanentApplied && state.PermanentSince.IsZero() && strings.ToLower(strings.TrimSpace(actualDuration)) == "permanent" {
				state.PermanentSince = now
			}
		}
		p.eventsMu.Unlock()
	}
	if logCleared {
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "violator_cleared",
			Message:   fmt.Sprintf("Статус нарушителя снят: hits=%d, sources=%d/%d, duration=%s", clearHits, sourceCount, limit, clearDuration.Truncate(time.Second)),
		})
	}
	if sendPermanentAlert {
		p.enqueueSideEffectTask(func() {
			if err := p.alerter.SendAlert(permanentAlertPayload); err != nil {
				p.addHistory(userEmail, models.UserEventLog{
					Timestamp: time.Now(),
					Type:      "alert_failed_permanent",
					Message:   "Ошибка webhook (permanent): " + err.Error(),
					SourceIP:  sourceIP,
					NodeName:  nodeName,
				})
				log.Printf("Ошибка отправки webhook-permanent уведомления: %v", err)
				return
			}
			p.addHistory(userEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "alert_sent_permanent",
				Message:   "Webhook-уведомление о перманентном бане отправлено",
				SourceIP:  sourceIP,
				NodeName:  nodeName,
			})
		})
	}
	if logGeoMismatch {
		sort.Strings(geoMismatchCountries)
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "geo_sharing_detected",
			Message:   fmt.Sprintf("Обнаружен геошаринг: IP из разных стран [%s]", strings.Join(geoMismatchCountries, ", ")),
			SourceIP:  sourceIP,
			NodeName:  nodeName,
		})
		log.Printf("ГЕО-ШАРИНГ: %s — IP из разных стран: %s", userEmail, strings.Join(geoMismatchCountries, ", "))
	}
}

// GetSharingStatus возвращает статус детекции шаринга по пользователю.
func (p *LogProcessor) GetSharingStatus(userEmail string) (models.SharingStatus, bool) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return models.SharingStatus{}, false
	}

	now := time.Now()
	triggerCount := p.sharingTriggerCount()
	triggerPeriod := p.sharingTriggerPeriod()

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	state := p.sharingState[userKey]
	if state == nil {
		return models.SharingStatus{}, false
	}
	p.pruneSharingTriggersLocked(state, now, triggerPeriod)
	if !state.ViolatorSince.IsZero() && len(state.Triggers) < triggerCount {
		state.ViolatorSince = time.Time{}
		if state.PermanentSince.IsZero() {
			state.BanlistSince = time.Time{}
			state.ViolationIPs = nil
			state.ViolationNodes = nil
		}
	}

	limit := p.getUserIPLimit(context.Background(), userEmail)
	sourceCount := state.LastSourceCount
	if sourceCount < 0 {
		sourceCount = 0
	}
	return p.sharingStatusLocked(userKey, state, sourceCount, limit, now), true
}

// ListSharingStatuses возвращает статусы шаринга по всем пользователям.
func (p *LogProcessor) ListSharingStatuses(limit int) []models.SharingStatus {
	if limit <= 0 {
		limit = 2000
	}

	now := time.Now()
	triggerCount := p.sharingTriggerCount()
	triggerPeriod := p.sharingTriggerPeriod()

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	rows := make([]models.SharingStatus, 0, len(p.sharingState))
	for userKey, state := range p.sharingState {
		if len(rows) >= limit {
			break
		}
		if state == nil {
			continue
		}
		p.pruneSharingTriggersLocked(state, now, triggerPeriod)
		if !state.ViolatorSince.IsZero() && len(state.Triggers) < triggerCount {
			state.ViolatorSince = time.Time{}
			if state.PermanentSince.IsZero() {
				state.BanlistSince = time.Time{}
				state.ViolationIPs = nil
				state.ViolationNodes = nil
			}
		}
		limitForUser := p.getUserIPLimit(context.Background(), userKey)
		sourceCount := state.LastSourceCount
		if sourceCount < 0 {
			sourceCount = 0
		}
		rows = append(rows, p.sharingStatusLocked(userKey, state, sourceCount, limitForUser, now))
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].InBanlist != rows[j].InBanlist {
			return rows[i].InBanlist
		}
		if rows[i].Violator != rows[j].Violator {
			return rows[i].Violator
		}
		if rows[i].TriggerHits != rows[j].TriggerHits {
			return rows[i].TriggerHits > rows[j].TriggerHits
		}
		return rows[i].UserIdentifier < rows[j].UserIdentifier
	})
	return rows
}

func (p *LogProcessor) sharingStatusLocked(userKey string, state *sharingState, sourceCount, limit int, now time.Time) models.SharingStatus {
	triggerCount := p.sharingTriggerCount()
	triggerPeriod := p.sharingTriggerPeriod()
	triggerHits := minInt(len(state.Triggers), triggerCount)
	permanentBan := !state.PermanentSince.IsZero()
	banlist := !state.BanlistSince.IsZero() || permanentBan
	violator := !state.ViolatorSince.IsZero()

	ips := make([]string, 0, len(state.ViolationIPs))
	for ip := range state.ViolationIPs {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	nodes := make([]string, 0, len(state.ViolationNodes))
	for node := range state.ViolationNodes {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	durationSec := 0
	if violator {
		durationSec = int(now.Sub(state.ViolatorSince).Seconds())
		if durationSec < 0 {
			durationSec = 0
		}
	}

	geoCountries := make([]string, 0, len(state.CountrySeen))
	for c := range state.CountrySeen {
		geoCountries = append(geoCountries, c)
	}
	sort.Strings(geoCountries)
	minCountries := p.cfg.GeoSharingMinCountries
	if minCountries <= 0 {
		minCountries = 2
	}
	geoMismatch := p.cfg.GeoSharingEnabled && len(geoCountries) >= minCountries

	return models.SharingStatus{
		UserIdentifier:         userKey,
		ConcurrentIPs:          sourceCount,
		ConcurrentLimit:        limit,
		TriggerHits:            triggerHits,
		TriggerRequired:        triggerCount,
		TriggerPeriodSeconds:   int(triggerPeriod.Seconds()),
		Violator:               violator,
		ViolatorSince:          state.ViolatorSince,
		InBanlist:              banlist,
		BanlistSince:           state.BanlistSince,
		PermanentBan:           permanentBan,
		PermanentBanSince:      state.PermanentSince,
		ViolationDurationSec:   durationSec,
		ViolationIPs:           ips,
		ViolationNodes:         nodes,
		LastTriggerAt:          state.LastTriggerAt,
		CurrentExceedsInWindow: sourceCount > limit && limit > 0,
		BanCount:               state.BanCount,
		GeoCountries:           geoCountries,
		GeoMismatch:            geoMismatch,
	}
}

func (p *LogProcessor) countConcurrentSourcesLocked(userKey string, now time.Time) int {
	window := p.sharingConcurrentWindow()
	cutoff := now.Add(-window)
	uniq := make(map[string]struct{})
	for _, event := range p.recentEvents[userKey] {
		if event.SourceIP == "" {
			continue
		}
		if event.SeenAt.Before(cutoff) {
			continue
		}
		key := sharingSourceKey(event.SourceIP, p.cfg.SubnetGrouping)
		if key == "" {
			continue
		}
		uniq[key] = struct{}{}
	}
	return len(uniq)
}

func (p *LogProcessor) pruneSharingTriggersLocked(state *sharingState, now time.Time, period time.Duration) {
	if state == nil || len(state.Triggers) == 0 {
		return
	}
	cutoff := now.Add(-period)
	dst := state.Triggers[:0]
	for _, ts := range state.Triggers {
		if ts.After(cutoff) || ts.Equal(cutoff) {
			dst = append(dst, ts)
		}
	}
	state.Triggers = dst
}

func (p *LogProcessor) sharingConcurrentWindow() time.Duration {
	if p.cfg.ConcurrentWindow <= 0 {
		return 2 * time.Second
	}
	return p.cfg.ConcurrentWindow
}

func (p *LogProcessor) sharingSourceWindow() time.Duration {
	if p.cfg.SharingSourceWindow <= 0 {
		return 3 * time.Minute
	}
	return p.cfg.SharingSourceWindow
}

func (p *LogProcessor) sharingSustainDuration() time.Duration {
	if p.cfg.SharingSustain <= 0 {
		return 90 * time.Second
	}
	return p.cfg.SharingSustain
}

func (p *LogProcessor) sharingTriggerPeriod() time.Duration {
	if p.cfg.TriggerPeriod <= 0 {
		return 30 * time.Second
	}
	return p.cfg.TriggerPeriod
}

func (p *LogProcessor) sharingTriggerCount() int {
	if p.cfg.TriggerCount < 1 {
		return 5
	}
	return p.cfg.TriggerCount
}

func (p *LogProcessor) sharingTriggerMinInterval() time.Duration {
	if p.cfg.TriggerMinInterval <= 0 {
		return 6 * time.Second
	}
	return p.cfg.TriggerMinInterval
}

func (p *LogProcessor) sharingBanlistThreshold() time.Duration {
	if p.cfg.BanlistThreshold <= 0 {
		return 5 * time.Minute
	}
	return p.cfg.BanlistThreshold
}

// getEscalatedDuration возвращает длительность блокировки с учётом числа предыдущих банов.
// Если BAN_ESCALATION_ENABLED=true, использует лестницу из BAN_ESCALATION_DURATIONS.
func (p *LogProcessor) getEscalatedDuration(banCount int) string {
	if p.cfg.BanEscalationEnabled && len(p.cfg.BanEscalationDurations) > 0 {
		idx := banCount - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(p.cfg.BanEscalationDurations) {
			idx = len(p.cfg.BanEscalationDurations) - 1
		}
		if d := strings.TrimSpace(p.cfg.BanEscalationDurations[idx]); d != "" {
			return d
		}
	}
	d := strings.TrimSpace(p.cfg.SharingPermanentBanDuration)
	if d == "" {
		return "permanent"
	}
	return d
}

func (p *LogProcessor) buildHeuristicMetrics(userEmail string, candidateIPs []string) models.HeuristicMetrics {
	userKey := normalizeUserID(userEmail)
	now := time.Now()

	metrics := models.HeuristicMetrics{
		PassedHeuristics: true,
	}

	p.eventsMu.Lock()
	events := p.pruneEventsLocked(p.recentEvents[userKey], now)
	p.recentEvents[userKey] = events
	p.eventsMu.Unlock()

	if len(events) == 0 {
		p.storeHeuristicMetrics(userKey, metrics)
		return metrics
	}

	maxSamples := p.cfg.HeuristicsMaxSamples
	if maxSamples <= 0 {
		maxSamples = 30
	}
	if len(events) > maxSamples {
		events = events[len(events)-maxSamples:]
	}

	uniqueIPs := make(map[string]struct{})
	subnetSource := candidateIPs
	if len(subnetSource) == 0 {
		subnetSource = make([]string, 0, len(events))
	}

	lastNode := "unknown"
	lastEventAt := events[len(events)-1].SeenAt
	switches := 0
	prevIP := ""
	for i, event := range events {
		if event.SourceIP != "" {
			uniqueIPs[event.SourceIP] = struct{}{}
			if len(candidateIPs) == 0 {
				subnetSource = append(subnetSource, event.SourceIP)
			}
		}
		if event.NodeName != "" {
			lastNode = event.NodeName
		}
		if i > 0 && event.SourceIP != "" && prevIP != "" && event.SourceIP != prevIP {
			switches++
		}
		if event.SourceIP != "" {
			prevIP = event.SourceIP
		}
	}

	samples := len(events)
	switchRate := 0.0
	if samples > 1 {
		switchRate = float64(switches) / float64(samples-1)
	}

	diversity := 0.0
	if samples > 0 {
		diversity = float64(len(uniqueIPs)) / float64(samples)
	}

	metrics = models.HeuristicMetrics{
		Samples:        samples,
		UniqueIPs:      len(uniqueIPs),
		UniqueSubnet24: countUniqueSubnet24(subnetSource),
		SwitchRate:     switchRate,
		DiversityRatio: diversity,
		LastNode:       lastNode,
		LastEventAt:    lastEventAt,
	}

	metrics.PassedHeuristics = p.shouldPassHeuristics(metrics)
	p.storeHeuristicMetrics(userKey, metrics)
	return metrics
}

func (p *LogProcessor) shouldPassHeuristics(metrics models.HeuristicMetrics) bool {
	if !p.cfg.HeuristicsEnabled {
		return true
	}
	if metrics.Samples < p.cfg.HeuristicsMinSamples {
		return true
	}
	if metrics.UniqueSubnet24 < p.cfg.HeuristicsMinSubnet24 {
		return false
	}
	return metrics.SwitchRate >= p.cfg.HeuristicsMinSwitchRate || metrics.DiversityRatio >= p.cfg.HeuristicsMinDiversityRatio
}

func (p *LogProcessor) pruneEventsLocked(events []userEvent, now time.Time) []userEvent {
	if len(events) == 0 {
		return events
	}
	cutoff := now.Add(-p.cfg.HeuristicsWindow)
	pruned := events[:0]
	for _, event := range events {
		if event.SeenAt.After(cutoff) || event.SeenAt.Equal(cutoff) {
			pruned = append(pruned, event)
		}
	}
	return pruned
}

func (p *LogProcessor) storeHeuristicMetrics(userKey string, metrics models.HeuristicMetrics) {
	if userKey == "" {
		return
	}
	p.eventsMu.Lock()
	p.heuristics[userKey] = metrics
	p.eventsMu.Unlock()
}

// GetHeuristicMetrics возвращает последние рассчитанные метрики пользователя.
func (p *LogProcessor) GetHeuristicMetrics(userEmail string) (models.HeuristicMetrics, bool) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return models.HeuristicMetrics{}, false
	}

	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	metrics, ok := p.heuristics[userKey]
	return metrics, ok
}

// GetLastNode возвращает последнюю ноду, откуда был входящий трафик пользователя.
func (p *LogProcessor) GetLastNode(userEmail string) string {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return "unknown"
	}

	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	if node := strings.TrimSpace(p.lastNode[userKey]); node != "" {
		return node
	}

	if metrics, ok := p.heuristics[userKey]; ok && strings.TrimSpace(metrics.LastNode) != "" {
		return metrics.LastNode
	}

	return "unknown"
}

// GetLastSeenAt возвращает время последней активности пользователя.
func (p *LogProcessor) GetLastSeenAt(userEmail string) (time.Time, bool) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return time.Time{}, false
	}

	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	lastSeen, ok := p.lastSeenAt[userKey]
	if !ok || lastSeen.IsZero() {
		return time.Time{}, false
	}
	return lastSeen, true
}

// GetQueueStats возвращает текущую загрузку очередей обработчика.
func (p *LogProcessor) GetQueueStats() (logQueueLen int, sideEffectQueueLen int) {
	return len(p.logChannel), len(p.sideEffectChannel)
}

// GetUserHistory возвращает историю событий пользователя в обратном хронологическом порядке.
func (p *LogProcessor) GetUserHistory(userEmail string, limit int) []models.UserEventLog {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}

	p.eventsMu.RLock()
	src := append([]models.UserEventLog(nil), p.history[userKey]...)
	p.eventsMu.RUnlock()

	if len(src) == 0 {
		return nil
	}

	if len(src) > limit {
		src = src[len(src)-limit:]
	}

	for i, j := 0, len(src)-1; i < j; i, j = i+1, j-1 {
		src[i], src[j] = src[j], src[i]
	}
	return src
}

// GetGlobalHistory возвращает общий журнал событий по всем пользователям в обратном хронологическом порядке.
func (p *LogProcessor) GetGlobalHistory(limit int) []models.GlobalUserEventLog {
	if limit <= 0 {
		limit = 500
	}

	p.eventsMu.RLock()
	all := make([]models.GlobalUserEventLog, 0, 1024)
	for userID, history := range p.history {
		for _, event := range history {
			all = append(all, models.GlobalUserEventLog{
				UserIdentifier: userID,
				Timestamp:      event.Timestamp,
				Type:           event.Type,
				Message:        event.Message,
				SourceIP:       event.SourceIP,
				NodeName:       event.NodeName,
			})
		}
	}
	p.eventsMu.RUnlock()

	if len(all) == 0 {
		return nil
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.After(all[j].Timestamp)
	})

	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// ApplyBlockerReport применяет ack/heartbeat от blocker и обновляет runtime-состояние.
func (p *LogProcessor) ApplyBlockerReport(report models.BlockerReport) {
	node := strings.TrimSpace(report.NodeName)
	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now()
	}

	reportType := strings.ToLower(strings.TrimSpace(report.Type))
	action := strings.ToLower(strings.TrimSpace(report.Action))
	ip := strings.TrimSpace(report.IP)

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()

	if node != "" {
		st := p.nodeState[node]
		if reportType == "heartbeat" {
			st.LastBlockerBeatAt = report.Timestamp
		} else {
			st.LastBlockerActionAt = report.Timestamp
			st.LastBlockerBeatAt = report.Timestamp
		}
		p.nodeState[node] = st
	}

	if reportType == "heartbeat" {
		return
	}

	if ip == "" || !report.Success {
		return
	}

	switch action {
	case "unblock":
		delete(p.activeBans, ip)
	case "block":
		expiresAt := time.Time{}
		if dur, ok := parseFlexibleDuration(strings.TrimSpace(report.Duration)); ok && dur > 0 {
			expiresAt = report.Timestamp.Add(dur)
		}
		p.activeBans[ip] = activeBanState{
			NodeName:  node,
			Duration:  strings.TrimSpace(report.Duration),
			SetAt:     report.Timestamp,
			ExpiresAt: expiresAt,
		}
	}
	p.cleanupExpiredBansLocked(time.Now())
}

// RecordNodeHeartbeat обновляет heartbeat узла (например, от forwarder).
func (p *LogProcessor) RecordNodeHeartbeat(nodeName string, seenAt time.Time) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}

	p.eventsMu.Lock()
	st := p.nodeState[nodeName]
	st.LastTrafficAt = seenAt
	p.nodeState[nodeName] = st
	p.eventsMu.Unlock()
}

// GetActiveBans возвращает подтвержденные активные блокировки.
func (p *LogProcessor) GetActiveBans(limit int) []models.ActiveBanInfo {
	if limit <= 0 {
		limit = 500
	}

	now := time.Now()
	p.eventsMu.Lock()
	p.cleanupExpiredBansLocked(now)
	out := make([]models.ActiveBanInfo, 0, len(p.activeBans))
	for ip, ban := range p.activeBans {
		item := models.ActiveBanInfo{
			IP:       ip,
			NodeName: ban.NodeName,
			Duration: ban.Duration,
			SetAt:    ban.SetAt,
		}
		if !ban.ExpiresAt.IsZero() {
			item.ExpiresAt = ban.ExpiresAt
			if ban.ExpiresAt.After(now) {
				item.SecondsLeft = int(ban.ExpiresAt.Sub(now).Seconds())
			}
		}
		out = append(out, item)
	}
	p.eventsMu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].SetAt.After(out[j].SetAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// IsIPActivelyBanned возвращает true, если IP находится в подтвержденном активном бане.
func (p *LogProcessor) IsIPActivelyBanned(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	now := time.Now()
	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	p.cleanupExpiredBansLocked(now)
	_, ok := p.activeBans[ip]
	return ok
}

// GetNodeRuntimeStates возвращает срез runtime-состояний нод.
func (p *LogProcessor) GetNodeRuntimeStates() map[string]models.NodeRuntimeState {
	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	out := make(map[string]models.NodeRuntimeState, len(p.nodeState))
	for k, v := range p.nodeState {
		out[k] = v
	}
	return out
}

// RecordManualEvent пишет событие ручного действия оператора.
func (p *LogProcessor) RecordManualEvent(userEmail, eventType, message string) {
	p.addHistory(userEmail, models.UserEventLog{
		Timestamp: time.Now(),
		Type:      eventType,
		Message:   message,
	})
}

// ResetSharingState очищает накопленные триггеры/статус нарушителя для пользователя.
func (p *LogProcessor) ResetSharingState(userEmail, reason string) bool {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return false
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual_reset"
	}
	now := time.Now()

	var (
		hadState      bool
		hadViolator   bool
		previousHits  int
		clearDuration time.Duration
	)

	p.eventsMu.Lock()
	state := p.sharingState[userKey]
	if state != nil {
		previousHits = len(state.Triggers)
		hadState = previousHits > 0 || !state.ExceedSince.IsZero() || !state.ViolatorSince.IsZero() || !state.BanlistSince.IsZero()
		hadViolator = !state.ViolatorSince.IsZero()
		if hadViolator {
			clearDuration = now.Sub(state.ViolatorSince)
			if clearDuration < 0 {
				clearDuration = 0
			}
		}

		state.Triggers = nil
		state.ExceedSince = time.Time{}
		state.LastTriggerAt = time.Time{}
		state.LastSourceCount = 0
		state.ViolatorSince = time.Time{}
		if state.PermanentSince.IsZero() {
			state.BanlistSince = time.Time{}
			state.ViolationIPs = nil
			state.ViolationNodes = nil
		}
	}
	p.eventsMu.Unlock()

	if hadState {
		msg := fmt.Sprintf("Статус нарушителя сброшен вручную: reason=%s, hits=%d", reason, previousHits)
		if hadViolator {
			msg += fmt.Sprintf(", duration=%s", clearDuration.Truncate(time.Second))
		}
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: now,
			Type:      "violator_cleared",
			Message:   msg,
		})
	}
	return hadState
}

// RecordBlockedIPs сохраняет последние IP, отправленные на блокировку для пользователя.
func (p *LogProcessor) RecordBlockedIPs(userEmail string, ips []string) {
	userKey := normalizeUserID(userEmail)
	if userKey == "" || len(ips) == 0 {
		return
	}

	uniq := make(map[string]struct{}, len(ips))
	clean := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, exists := uniq[ip]; exists {
			continue
		}
		uniq[ip] = struct{}{}
		clean = append(clean, ip)
	}
	if len(clean) == 0 {
		return
	}
	sort.Strings(clean)

	p.eventsMu.Lock()
	p.lastBlockedIPs[userKey] = clean
	p.eventsMu.Unlock()
}

// GetLastBlockedIPs возвращает последние IP пользователя, которые отправлялись на блокировку.
func (p *LogProcessor) GetLastBlockedIPs(userEmail string) []string {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return nil
	}

	p.eventsMu.RLock()
	defer p.eventsMu.RUnlock()

	ips := p.lastBlockedIPs[userKey]
	if len(ips) == 0 {
		return nil
	}
	return append([]string(nil), ips...)
}

func (p *LogProcessor) filterExcludedIPs(ips []string, email string) []string {
	var filtered []string
	for _, ip := range ips {
		if p.cfg.ExcludedIPs[ip] {
			log.Printf("IP-адрес %s для пользователя %s пропущен, так как находится в списке исключений.", ip, email)
			continue
		}
		filtered = append(filtered, ip)
	}
	return filtered
}

func (p *LogProcessor) scheduleIPsClear(ctx context.Context, userEmail string) {
	log.Printf("Планирование отложенной очистки IP для %s через %v.", userEmail, p.cfg.ClearIPsDelay)
	p.addHistory(userEmail, models.UserEventLog{
		Timestamp: time.Now(),
		Type:      "clear_scheduled",
		Message:   "Плановая очистка IP запланирована",
	})

	time.AfterFunc(p.cfg.ClearIPsDelay, func() {
		if ctx.Err() != nil {
			p.addHistory(userEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "clear_canceled",
				Message:   "Очистка отменена из-за остановки сервиса",
			})
			log.Printf("Отложенная очистка IP для %s отменена из-за остановки сервиса.", userEmail)
			return
		}

		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		cleared, err := p.storage.ClearUserIPs(opCtx, userEmail)
		if err != nil {
			p.addHistory(userEmail, models.UserEventLog{
				Timestamp: time.Now(),
				Type:      "clear_failed",
				Message:   "Очистка IP завершилась ошибкой: " + err.Error(),
			})
			if errors.Is(err, context.Canceled) {
				log.Printf("Отложенная очистка IP для %s отменена из-за остановки сервиса во время выполнения.", userEmail)
			} else if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Таймаут при отложенной очистке IP для %s.", userEmail)
			} else {
				log.Printf("Ошибка при отложенной очистке IP для %s: %v", userEmail, err)
			}
			return
		}
		p.addHistory(userEmail, models.UserEventLog{
			Timestamp: time.Now(),
			Type:      "clear_done",
			Message:   "Очистка IP завершена",
		})
		log.Printf("Отложенная очистка IP для %s%s выполнена. Очищено ключей: %d",
			userEmail, p.getDebugMarker(userEmail), cleared)
	})
}

func normalizeUserID(user string) string {
	return strings.ToLower(strings.TrimSpace(user))
}

// loadPersistentBanCount загружает BanCount из Redis при первой инициализации sharingState.
func (p *LogProcessor) loadPersistentBanCount(userEmail, userKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count, err := p.storage.GetUserBanCount(ctx, userEmail)
	if err != nil || count == 0 {
		return
	}
	p.eventsMu.Lock()
	if st := p.sharingState[userKey]; st != nil && !st.BanCountLoaded {
		st.BanCount = count
		st.BanCountLoaded = true
	}
	p.eventsMu.Unlock()
}

// GetUserBlockCount возвращает персистентный счётчик блокировок из Redis.
func (p *LogProcessor) GetUserBlockCount(ctx context.Context, userEmail string) int {
	count, _ := p.storage.GetUserBlockCount(ctx, userEmail)
	return count
}

func (p *LogProcessor) countEffectiveSharingSources(
	ctx context.Context,
	userEmail string,
	activeIPs []string,
	sourceIP string,
	now time.Time,
) int {
	userKey := normalizeUserID(userEmail)
	if userKey == "" {
		return 0
	}

	uniqueIPs := make(map[string]struct{}, len(activeIPs)+1)
	for _, ip := range activeIPs {
		ip = strings.TrimSpace(ip)
		if ip != "" {
			uniqueIPs[ip] = struct{}{}
		}
	}
	sourceIP = strings.TrimSpace(sourceIP)
	if sourceIP != "" {
		uniqueIPs[sourceIP] = struct{}{}
	}
	if len(uniqueIPs) == 0 {
		return 0
	}

	window := p.sharingSourceWindow()
	cutoff := now.Add(-window)
	recentSeenByIP := make(map[string]time.Time)

	p.eventsMu.RLock()
	for _, event := range p.recentEvents[userKey] {
		ip := strings.TrimSpace(event.SourceIP)
		if ip == "" || event.SeenAt.Before(cutoff) {
			continue
		}
		if prev, ok := recentSeenByIP[ip]; !ok || event.SeenAt.After(prev) {
			recentSeenByIP[ip] = event.SeenAt
		}
	}
	p.eventsMu.RUnlock()

	ips := make([]string, 0, len(uniqueIPs))
	for ip := range uniqueIPs {
		ips = append(ips, ip)
	}

	uniqSources := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		if ip != sourceIP {
			if seenAt, ok := recentSeenByIP[ip]; !ok || seenAt.Before(cutoff) {
				continue
			}
		}
		key := p.sharingSourceKeyWithContext(ctx, ip)
		if key == "" {
			continue
		}
		uniqSources[key] = struct{}{}
	}

	if len(uniqSources) == 0 && sourceIP != "" {
		if fallback := p.sharingSourceKeyWithContext(ctx, sourceIP); fallback != "" {
			uniqSources[fallback] = struct{}{}
		}
	}

	return len(uniqSources)
}

func (p *LogProcessor) sharingSourceKeyWithContext(ctx context.Context, ip string) string {
	base := sharingSourceKey(ip, p.cfg.SubnetGrouping)
	if base == "" {
		return ""
	}

	info := p.classifyIP(ctx, ip)
	if normalizeNetworkTypeLocal(info.Type) != "mobile" {
		return base
	}

	mobileKey := sharingMobileSourceKey(ip, info.Provider, info.Organization, info.Country)
	if mobileKey == "" {
		return base
	}
	return mobileKey
}

func countUniqueSubnet24(ips []string) int {
	subnets := make(map[string]struct{})
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			subnets[parts[0]+"."+parts[1]+"."+parts[2]] = struct{}{}
			continue
		}
		subnets[ip] = struct{}{}
	}
	return len(subnets)
}

func sharingSourceKey(ip string, subnetGrouping bool) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	if !subnetGrouping {
		return ip
	}
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
	}
	return ip
}

func countUniqueSharingSources(ips []string, subnetGrouping bool) int {
	if len(ips) == 0 {
		return 0
	}
	uniq := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		key := sharingSourceKey(ip, subnetGrouping)
		if key == "" {
			continue
		}
		uniq[key] = struct{}{}
	}
	return len(uniq)
}

func sharingMobileSourceKey(ip, provider, organization, country string) string {
	prefix := sharingMobilePrefix(ip)
	if prefix == "" {
		return ""
	}

	providerToken := normalizeSourceToken(provider)
	if providerToken == "" {
		providerToken = normalizeSourceToken(organization)
	}
	if providerToken == "" {
		providerToken = "unknown"
	}

	countryToken := normalizeSourceToken(country)
	if countryToken == "" {
		countryToken = "xx"
	}

	return "mobile:" + providerToken + ":" + countryToken + ":" + prefix
}

func sharingMobilePrefix(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return ""
	}

	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d", v4[0], v4[1])
	}

	masked := parsed.Mask(net.CIDRMask(48, 128))
	if masked == nil {
		return ""
	}
	return strings.ToLower(masked.String())
}

func normalizeSourceToken(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}

	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func (p *LogProcessor) cleanupExpiredBansLocked(now time.Time) {
	for ip, ban := range p.activeBans {
		if !ban.ExpiresAt.IsZero() && !ban.ExpiresAt.After(now) {
			delete(p.activeBans, ip)
		}
	}
}

func parseFlexibleDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return 0, false
	}
	if strings.HasSuffix(raw, "d") {
		daysRaw := strings.TrimSuffix(raw, "d")
		if daysRaw == "" {
			return 0, false
		}
		days, err := time.ParseDuration(daysRaw + "h")
		if err != nil {
			return 0, false
		}
		return days * 24, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}

func (p *LogProcessor) classifyIP(ctx context.Context, ip string) models.NetworkInfo {
	if p.networkClassifier == nil {
		return models.NetworkInfo{Type: "unknown"}
	}
	info := p.networkClassifier.Classify(ctx, ip)
	if strings.TrimSpace(info.Type) == "" {
		info.Type = "unknown"
	}
	return info
}

func (p *LogProcessor) buildNetworkSummary(ctx context.Context, ips []string) networkSummary {
	summary := networkSummary{
		DominantType: "unknown",
		Counts:       make(map[string]int),
		Total:        0,
	}
	if len(ips) == 0 {
		return summary
	}

	for _, ip := range ips {
		info := p.classifyIP(ctx, ip)
		typ := normalizeNetworkTypeLocal(info.Type)
		summary.Counts[typ]++
		summary.Total++
	}
	if summary.Total == 0 {
		return summary
	}

	bestType := "unknown"
	bestCount := -1
	for _, typ := range []string{"mobile", "wifi", "cable", "unknown"} {
		count := summary.Counts[typ]
		if count > bestCount {
			bestType = typ
			bestCount = count
		}
	}
	summary.DominantType = bestType
	return summary
}

func (p *LogProcessor) shouldBlockByNetworkPolicy(baseLimit, currentCount int, summary networkSummary) (bool, string) {
	if !p.cfg.NetworkPolicyEnabled {
		return true, "network_policy_disabled"
	}
	if baseLimit <= 0 || currentCount <= 0 {
		return true, "invalid_limits"
	}

	forceExcess := p.cfg.NetworkForceBlockExcessIPs
	if forceExcess < 1 {
		forceExcess = 1
	}
	forceThreshold := baseLimit + forceExcess
	if currentCount >= forceThreshold {
		return true, fmt.Sprintf("force_block: ip_count=%d threshold=%d", currentCount, forceThreshold)
	}

	grace := p.networkGraceBySummary(summary)
	effectiveLimit := baseLimit + grace
	if currentCount <= effectiveLimit {
		return false, fmt.Sprintf(
			"grace_applied: type=%s ip_count=%d effective_limit=%d base=%d grace=%d",
			summary.DominantType,
			currentCount,
			effectiveLimit,
			baseLimit,
			grace,
		)
	}

	return true, fmt.Sprintf(
		"policy_pass: type=%s ip_count=%d effective_limit=%d",
		summary.DominantType,
		currentCount,
		effectiveLimit,
	)
}

func (p *LogProcessor) networkGraceBySummary(summary networkSummary) int {
	dominant := normalizeNetworkTypeLocal(summary.DominantType)
	if summary.Total > 0 {
		dominantCount := summary.Counts[dominant]
		if dominantCount*100/summary.Total < 60 {
			return maxInt(0, p.cfg.NetworkMixedGraceIPs)
		}
	}

	switch dominant {
	case "mobile":
		return maxInt(0, p.cfg.NetworkMobileGraceIPs)
	case "wifi":
		return maxInt(0, p.cfg.NetworkWifiGraceIPs)
	case "cable":
		return maxInt(0, p.cfg.NetworkCableGraceIPs)
	default:
		return maxInt(0, p.cfg.NetworkUnknownGraceIPs)
	}
}

func normalizeNetworkTypeLocal(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "mobile", "wifi", "cable":
		return strings.TrimSpace(strings.ToLower(raw))
	default:
		return "unknown"
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
