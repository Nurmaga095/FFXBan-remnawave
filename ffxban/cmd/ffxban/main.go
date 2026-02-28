package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ffxban/internal/api"
	"ffxban/internal/config"
	"ffxban/internal/monitor"
	"ffxban/internal/processor"
	"ffxban/internal/services/alerter"
	"ffxban/internal/services/blocker"
	"ffxban/internal/services/netclass"
	"ffxban/internal/services/panel"
	"ffxban/internal/services/publisher"
	"ffxban/internal/services/status"
	"ffxban/internal/services/storage"
)

var buildVersion = "1.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg := config.New()

	// Контекст для сигнализации о завершении работы фоновых процессов
	ctx, cancel := context.WithCancel(context.Background())
	// WaitGroup для ожидания завершения всех фоновых горутин
	var wg sync.WaitGroup

	redisStore, err := storage.NewRedisStore(ctx, cfg.RedisURL, "internal/scripts/add_and_check_ip.lua")
	if err != nil {
		log.Fatalf("Критическая ошибка: не удалось подключиться к Redis: %v", err)
	}
	defer redisStore.Close()

	rabbitPublisher, err := publisher.NewRabbitMQPublisher(cfg.RabbitMQURL, cfg.BlockingExchangeName)
	if err != nil {
		log.Fatalf("Critical error: RabbitMQ connection failed: %v", err)
	}
	defer rabbitPublisher.Close()

	var activeBlocker blocker.Blocker
	var rabbitBlocker *blocker.RabbitMQBlocker

	if rabbitPublisher != nil {
		rabbitBlocker = blocker.NewRabbitMQBlocker(rabbitPublisher)
	}

	if rabbitBlocker == nil {
		log.Fatalf("RabbitMQ blocker is not initialized")
	}
	activeBlocker = rabbitBlocker
	log.Println("Enabled Remnawave/RabbitMQ mode.")

	webhookAlerter := alerter.NewWebhookAlerter(
		cfg.AlertWebhookURL,
		cfg.AlertWebhookMode,
		cfg.AlertWebhookToken,
		cfg.AlertWebhookAuthHeader,
		cfg.AlertWebhookUsernamePrefix,
	)
	networkDetector := netclass.NewDetector(cfg)
	var limitProvider panel.UserLimitProvider
	var panelClient *panel.Client
	if cfg.PanelURL != "" && cfg.PanelToken != "" {
		panelClient = panel.NewClient(cfg.PanelURL, cfg.PanelToken, cfg.PanelReloadInterval, cfg.PerDeviceKeyDelimiter)
		limitProvider = panelClient
	}

	logProcessor := processor.NewLogProcessor(redisStore, activeBlocker, webhookAlerter, limitProvider, networkDetector, cfg)
	poolMonitor := monitor.NewPoolMonitor(redisStore, limitProvider, cfg)
	statusConsumer := status.NewConsumer(cfg.RabbitMQURL, cfg.BlockingStatusExchangeName, logProcessor.ApplyBlockerReport)
	apiServer := api.NewServer(cfg, logProcessor, redisStore, activeBlocker, limitProvider, networkDetector, buildVersion)

	backgroundWorkers := 4
	if panelClient != nil {
		backgroundWorkers++
	}

	wg.Add(backgroundWorkers)
	go poolMonitor.Run(ctx, &wg)
	go logProcessor.StartWorkerPool(ctx, &wg)
	go logProcessor.StartSideEffectWorkerPool(ctx, &wg) // Запускаем новый пул воркеров
	go statusConsumer.Run(ctx, &wg)
	if panelClient != nil {
		go panelClient.Run(ctx, &wg)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           apiServer.GetRouter(), // Получаем роутер из нашего api.Server
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSH exec может формировать большой финальный JSON-ответ и занимать минуты.
		// Короткий WriteTimeout приводит к обрыву ответа и HTTP 502 на nginx.
		WriteTimeout:      12 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("Сервер ffxban запущен на порту %s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)
	<-quit

	log.Println("Получен сигнал завершения, начинаю остановку сервиса...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Ошибка при остановке HTTP-сервера: %v", err)
	} else {
		log.Println("HTTP-сервер успешно остановлен.")
	}

	log.Println("Ожидание опустошения очередей процессора...")
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Second)
	drained := logProcessor.WaitDrained(drainCtx, 600*time.Millisecond)
	drainCancel()
	if drained {
		log.Println("Очереди процессора успешно опустошены.")
	} else {
		log.Println("Таймаут ожидания опустошения очередей. Продолжаю остановку.")
	}

	cancel()

	log.Println("Ожидание завершения фоновых процессов...")
	wg.Wait()

	log.Println("Все фоновые процессы остановлены. Сервис успешно остановлен.")
}
