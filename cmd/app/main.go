package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/TicketsBot/subscriptions-app/internal/config"
	"github.com/TicketsBot/subscriptions-app/internal/server"
	"github.com/TicketsBot/subscriptions-app/pkg/patreon"
	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx"
	"github.com/jackc/pgx/v4/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	_ "github.com/joho/godotenv/autoload"
)

func DbConn(conf config.Config, logger *zap.Logger) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf(
		"postgres://%s:%s@%s/%s?pool_max_conns=%d",
		conf.Database.Username,
		conf.Database.Password,
		conf.Database.Host,
		conf.Database.Database,
		conf.Database.Threads,
	))

	if err != nil {
		logger.Fatal("Failed to parse database config", zap.Error(err))
		return nil
	}

	// TODO: Sentry
	cfg.ConnConfig.LogLevel = pgx.LogLevelWarn

	pool, err := pgxpool.ConnectConfig(context.Background(), cfg)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
		return nil
	}

	return pool
}

func main() {
	conf, err := config.LoadConfig()
	if err != nil {
		panic(err)
	}

	var logger *zap.Logger
	if conf.ProductionMode {
		if conf.SentryDsn != nil {
			if err := sentry.Init(sentry.ClientOptions{
				Dsn: *conf.SentryDsn,
			}); err != nil {
				panic(err)
			}

			defer sentry.Flush(time.Second * 2)

			logger, err = zap.NewProduction(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
				return zapcore.RegisterHooks(core, func(entry zapcore.Entry) error {
					if entry.Level == zapcore.ErrorLevel {
						hostname, _ := os.Hostname()

						sentry.CaptureEvent(&sentry.Event{
							Extra: map[string]any{
								"caller": entry.Caller.String(),
								"stack":  entry.Stack,
							},
							Level:      sentry.LevelError,
							Message:    entry.Message,
							ServerName: hostname,
							Timestamp:  entry.Time,
							Logger:     entry.LoggerName,
						})
					}

					return nil
				})
			}))
		} else {
			logger, err = zap.NewProduction()
		}
	} else {
		logger, err = zap.NewDevelopment()
	}

	if err != nil {
		panic(err)
	}

	dbConn := DbConn(conf, logger)

	patreonClient := patreon.NewClient(conf, logger.With(zap.String("component", "patreon_client")), dbConn)

	pledgeCh := make(chan map[string]patreon.Patron)
	go startPatreonLoop(context.Background(), logger, patreonClient, pledgeCh)

	server := server.NewServer(conf, logger.With(zap.String("component", "server")))

	go func() {
		for pledges := range pledgeCh {
			server.UpdatePledges(pledges)
		}
	}()

	if err := server.Run(); err != nil {
		panic(err)
	}
}

func startPatreonLoop(ctx context.Context, logger *zap.Logger, patreonClient *patreon.Client, ch chan map[string]patreon.Patron) {
	for {
		fetchPledges(ctx, logger, patreonClient, ch)
		time.Sleep(time.Minute)
	}
}

func fetchPledges(
	ctx context.Context,
	logger *zap.Logger,
	patreonClient *patreon.Client,
	ch chan map[string]patreon.Patron,
) {
	if patreonClient.Tokens.ExpiresAt.Before(time.Now()) {
		logger.Fatal(
			"Refresh token has already expired (expired at %s)",
			zap.Time("expires_at", patreonClient.Tokens.ExpiresAt),
		)
		return
	}

	if time.Until(patreonClient.Tokens.ExpiresAt) < time.Hour*24*3 {
		logger.Info(
			"Token expires in less than 3 days, refreshing",
			zap.Time("expires_at", patreonClient.Tokens.ExpiresAt),
		)

		ctx, cancel := context.WithTimeout(ctx, time.Second*30)
		defer cancel()

		if err := patreonClient.RefreshCredentials(ctx); err != nil {
			logger.Error("Failed to refresh token", zap.Error(err))
		} else {
			logger.Info("Tokens refreshed successfully")
		}

		cancel()
	}

	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()

	pledges, err := patreonClient.FetchPledges(ctx)
	if err != nil {
		logger.Error("Failed to fetch pledges", zap.Error(err))
		return
	}

	ch <- pledges
}
