package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"
)

type TelegramReceiver struct {
	cfg     *Config
	db      *sql.DB
	bot     *tgbotapi.BotAPI
	logger  *zap.Logger
	metrics *MetricsCollector
}

func NewTelegramReceiver(cfg *Config, db *sql.DB, logger *zap.Logger, metrics *MetricsCollector) (*TelegramReceiver, error) {
	var bot *tgbotapi.BotAPI
	var err error

	if cfg.UseLocalBotAPI {
		bot, err = tgbotapi.NewBotAPI(cfg.TelegramBotToken)
		if err != nil {
			return nil, fmt.Errorf("failed to create bot API: %w", err)
		}
		bot.SetAPIEndpoint(cfg.LocalBotAPIURL + "/bot%s/%s")
	} else {
		bot, err = tgbotapi.NewBotAPI(cfg.TelegramBotToken)
		if err != nil {
			return nil, fmt.Errorf("failed to create bot API: %w", err)
		}
	}

	logger.Info("Telegram Bot connected", zap.String("username", bot.Self.UserName))

	return &TelegramReceiver{
		cfg:     cfg,
		db:      db,
		bot:     bot,
		logger:  logger,
		metrics: metrics,
	}, nil
}

func (tr *TelegramReceiver) Start(ctx context.Context) {
	tr.logger.Info("Telegram receiver starting")

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := tr.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			tr.logger.Info("Telegram receiver stopping")
			return
		case update := <-updates:
			if update.Message == nil {
				continue
			}

			go tr.handleMessage(update.Message)
		}
	}
}

func (tr *TelegramReceiver) handleMessage(msg *tgbotapi.Message) {
	// Check if user is admin
	if !tr.isAdmin(msg.From.ID) {
		tr.logger.Warn("Unauthorized access attempt",
			zap.Int64("user_id", msg.From.ID),
			zap.String("username", msg.From.UserName))

		reply := tgbotapi.NewMessage(msg.Chat.ID, "⛔ Unauthorized. This bot is admin-only.")
		tr.bot.Send(reply)
		return
	}

	// Handle commands
	if msg.IsCommand() {
		tr.handleCommand(msg)
		return
	}

	// Handle file uploads
	if msg.Document != nil {
		tr.handleDocument(msg)
		return
	}

	// Default response
	reply := tgbotapi.NewMessage(msg.Chat.ID, "📤 Send me archive files (ZIP, RAR) or TXT files to process.")
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleCommand(msg *tgbotapi.Message) {
	command := msg.Command()

	switch command {
	case "start":
		tr.handleStartCommand(msg)
	case "help":
		tr.handleHelpCommand(msg)
	case "queue":
		tr.handleQueueCommand(msg)
	case "batches":
		tr.handleBatchesCommand(msg)
	case "stats":
		tr.handleStatsCommand(msg)
	case "health":
		tr.handleHealthCommand(msg)
	default:
		reply := tgbotapi.NewMessage(msg.Chat.ID, "❓ Unknown command. Use /help for available commands.")
		tr.bot.Send(reply)
	}
}

func (tr *TelegramReceiver) handleStartCommand(msg *tgbotapi.Message) {
	text := `🤖 **Telegram Data Processor Bot - Option 2**

Welcome! This bot processes archive files (ZIP, RAR) and text files with batch-based parallel processing.

**Features:**
• 6× faster processing with parallel batches
• Supports files up to 4GB (Telegram Premium)
• Automatic extraction and conversion
• Real-time progress tracking

**Commands:**
/help - Show available commands
/queue - View queue status
/batches - View active batches
/stats - System statistics
/health - Health check

**Usage:**
Simply send your archive or text files, and I'll process them automatically!`

	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleHelpCommand(msg *tgbotapi.Message) {
	text := `📖 **Available Commands:**

*Status & Monitoring:*
/queue - Queue statistics (pending, downloading, downloaded)
/batches - Active batches and their status
/stats - System-wide statistics
/health - System health check

*File Processing:*
Just send archive files (ZIP, RAR) or TXT files directly!

**Supported File Types:**
• ZIP archives
• RAR archives
• TXT files

**File Size Limits:**
• Up to 4GB per file (requires Telegram Premium)`

	reply := tgbotapi.NewMessage(msg.Chat.ID, text)
	reply.ParseMode = "Markdown"
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleDocument(msg *tgbotapi.Message) {
	doc := msg.Document

	// Validate file type
	fileType := tr.getFileType(doc.FileName)
	if fileType == "" {
		reply := tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("❌ Unsupported file type: %s\n\nSupported: ZIP, RAR, TXT", doc.FileName))
		tr.bot.Send(reply)
		return
	}

	// Validate file size
	maxSizeBytes := tr.cfg.MaxFileSizeMB * 1024 * 1024
	if int64(doc.FileSize) > maxSizeBytes {
		reply := tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("❌ File too large: %.2f MB\n\nMaximum: %d MB",
				float64(doc.FileSize)/(1024*1024), tr.cfg.MaxFileSizeMB))
		tr.bot.Send(reply)
		return
	}

	// Add to download queue
	taskID, err := tr.addToQueue(msg.From.ID, doc.FileID, doc.FileName, fileType, int64(doc.FileSize))
	if err != nil {
		tr.logger.Error("Failed to add file to queue",
			zap.String("filename", doc.FileName),
			zap.Error(err))

		reply := tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("⚠️ Failed to queue file: %v", err))
		tr.bot.Send(reply)
		return
	}

	tr.logger.Info("File queued for download",
		zap.Int64("task_id", taskID),
		zap.String("filename", doc.FileName),
		zap.String("type", fileType),
		zap.Int64("size", int64(doc.FileSize)))

	// Send confirmation
	reply := tgbotapi.NewMessage(msg.Chat.ID,
		fmt.Sprintf("✅ File queued for processing\n\n"+
			"📁 **Filename:** %s\n"+
			"📊 **Size:** %.2f MB\n"+
			"🆔 **Task ID:** %d\n\n"+
			"Your file will be processed shortly. Use /queue to check status.",
			doc.FileName,
			float64(doc.FileSize)/(1024*1024),
			taskID))
	reply.ParseMode = "Markdown"
	tr.bot.Send(reply)

	// Record metrics
	tr.metrics.RecordFileSize(fileType, int64(doc.FileSize))
}

func (tr *TelegramReceiver) getFileType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".zip":
		return "ZIP"
	case ".rar":
		return "RAR"
	case ".txt":
		return "TXT"
	default:
		return ""
	}
}

func (tr *TelegramReceiver) addToQueue(userID int64, fileID, filename, fileType string, fileSize int64) (int64, error) {
	var taskID int64

	query := `
		INSERT INTO download_queue (file_id, user_id, filename, file_type, file_size, status)
		VALUES ($1, $2, $3, $4, $5, 'PENDING')
		RETURNING task_id
	`

	err := tr.db.QueryRow(query, fileID, userID, filename, fileType, fileSize).Scan(&taskID)
	if err != nil {
		return 0, fmt.Errorf("database insert failed: %w", err)
	}

	return taskID, nil
}

func (tr *TelegramReceiver) isAdmin(userID int64) bool {
	for _, adminID := range tr.cfg.AdminIDs {
		if adminID == userID {
			return true
		}
	}
	return false
}

// Command handlers (stubs for Phase 5)

func (tr *TelegramReceiver) handleQueueCommand(msg *tgbotapi.Message) {
	// TODO: Implement in Phase 5
	reply := tgbotapi.NewMessage(msg.Chat.ID, "📊 Queue command - to be implemented in Phase 5")
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleBatchesCommand(msg *tgbotapi.Message) {
	// TODO: Implement in Phase 5
	reply := tgbotapi.NewMessage(msg.Chat.ID, "🔄 Batches command - to be implemented in Phase 5")
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleStatsCommand(msg *tgbotapi.Message) {
	// TODO: Implement in Phase 5
	reply := tgbotapi.NewMessage(msg.Chat.ID, "📈 Stats command - to be implemented in Phase 5")
	tr.bot.Send(reply)
}

func (tr *TelegramReceiver) handleHealthCommand(msg *tgbotapi.Message) {
	// TODO: Implement in Phase 5
	reply := tgbotapi.NewMessage(msg.Chat.ID, "💚 Health command - to be implemented in Phase 5")
	tr.bot.Send(reply)
}
