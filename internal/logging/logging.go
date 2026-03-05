package logging

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

var Log zerolog.Logger

func Init(pretty bool) {
	if pretty {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

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
