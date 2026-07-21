package logger

import (
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// --- slog-адаптер для pion/logging ---

type slogLogger struct {
	l *slog.Logger
}

func (s slogLogger) Trace(msg string) { s.l.Debug(msg) }
func (s slogLogger) Tracef(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
func (s slogLogger) Debug(msg string) { s.l.Debug(msg) }
func (s slogLogger) Debugf(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
func (s slogLogger) Info(msg string)                          { s.l.Info(msg) }
func (s slogLogger) Infof(format string, args ...interface{}) { s.l.Info(fmt.Sprintf(format, args...)) }
func (s slogLogger) Warn(msg string)                          { s.l.Warn(msg) }
func (s slogLogger) Warnf(format string, args ...interface{}) { s.l.Warn(fmt.Sprintf(format, args...)) }
func (s slogLogger) Error(msg string)                         { s.l.Error(msg) }
func (s slogLogger) Errorf(format string, args ...interface{}) {
	s.l.Error(fmt.Sprintf(format, args...))
}

type SlogLoggerFactory struct {
	l *slog.Logger
}

func NewSlogLoggerFactory() SlogLoggerFactory {
	return SlogLoggerFactory{l: slog.Default()}
}

func (f SlogLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return slogLogger{l: f.l.With("component", scope)}
}
