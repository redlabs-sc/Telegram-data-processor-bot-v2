package main

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func InitLogger(cfg *Config) (*zap.Logger, error) {
	// Parse log level
	var level zapcore.Level
	switch cfg.LogLevel {
	case "debug":
		level = zapcore.DebugLevel
	case "info":
		level = zapcore.InfoLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		level = zapcore.InfoLevel
	}

	// Create encoder config
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	// Create encoder based on format
	var encoder zapcore.Encoder
	if cfg.LogFormat == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	// Create log file if specified
	var writeSyncer zapcore.WriteSyncer
	if cfg.LogFile != "" {
		// Ensure log directory exists
		if err := os.MkdirAll("logs", 0755); err != nil {
			return nil, err
		}

		file, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}

		// Write to both file and stdout
		writeSyncer = zapcore.NewMultiWriteSyncer(
			zapcore.AddSync(file),
			zapcore.AddSync(os.Stdout),
		)
	} else {
		// Write to stdout only
		writeSyncer = zapcore.AddSync(os.Stdout)
	}

	// Create core and logger
	core := zapcore.NewCore(encoder, writeSyncer, level)
	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))

	return logger, nil
}
