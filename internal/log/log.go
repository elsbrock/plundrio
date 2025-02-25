package log

import (
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

var log zerolog.Logger

// LogLevel represents the logging level
type LogLevel string

const (
	// Log levels
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
	LevelFatal LogLevel = "fatal"
	LevelNone  LogLevel = "none"
)

func init() {
	// Default to info level
	level := getLogLevel()

	// Configure zerolog
	zerolog.TimeFieldFormat = time.RFC3339

	// Initial logger setup
	configureLogger(level)
}

// configureLogger sets up the logger with the specified level
func configureLogger(level LogLevel) {
	// Configure output writer with colors enabled by default
	output := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
		NoColor:    false, // Always use colors
	}

	log = zerolog.New(output).With().Timestamp().Logger()

	// Set log level
	setLogLevel(level)
}

// getLogLevel determines the log level from environment
func getLogLevel() LogLevel {
	if envLevel := os.Getenv("PLDR_LOG_LEVEL"); envLevel != "" {
		return LogLevel(strings.ToLower(envLevel))
	}
	return LevelInfo
}

// setLogLevel sets the zerolog level
func setLogLevel(level LogLevel) {
	switch level {
	case LevelDebug:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case LevelInfo:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case LevelWarn:
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case LevelError:
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case LevelFatal:
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	case LevelNone:
		zerolog.SetGlobalLevel(zerolog.Disabled)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

// SetLevel sets the global log level
func SetLevel(level LogLevel) {
	// Reconfigure the logger
	configureLogger(level)
}

// Debug returns a new Debug level event logger with component context
func Debug(component string) *zerolog.Event {
	return log.Debug().Str("component", component)
}

// Info returns a new Info level event logger with component context
func Info(component string) *zerolog.Event {
	return log.Info().Str("component", component)
}

// Warn returns a new Warn level event logger with component context
func Warn(component string) *zerolog.Event {
	return log.Warn().Str("component", component)
}

// Error returns a new Error level event logger with component context
func Error(component string) *zerolog.Event {
	return log.Error().Str("component", component)
}

// Fatal returns a new Fatal level event logger with component context
func Fatal(component string) *zerolog.Event {
	return log.Fatal().Str("component", component)
}
