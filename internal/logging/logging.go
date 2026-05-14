package logging

import (
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

var Log zerolog.Logger

func Init(pretty bool) {
	level := zerolog.InfoLevel
	if pretty {
		level = zerolog.DebugLevel
	}

	if envLevel := os.Getenv("LOG_LEVEL"); envLevel != "" {
		if parsed, err := zerolog.ParseLevel(strings.ToLower(envLevel)); err == nil {
			level = parsed
		}
	}

	zerolog.SetGlobalLevel(level)

	if pretty {
		output := zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
			NoColor:    false,
		}
		Log = zerolog.New(output).With().Timestamp().Caller().Logger()
	} else {
		Log = zerolog.New(os.Stdout).With().Timestamp().Caller().Logger()
	}
}

func With() zerolog.Context {
	return Log.With()
}
