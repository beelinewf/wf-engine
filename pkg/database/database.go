// Package database opens and configures the PostgreSQL connection pool used
// by GORM. It contains no domain logic; it only turns Options into a ready
// *gorm.DB with the requested pool settings.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Options controls the PostgreSQL connection pool.
type Options struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	LogLevel        gormlogger.LogLevel
	// ConnectTimeout bounds gorm.Open in New; zero means no timeout.
	// It keeps a blackhole DSN from hanging the boot.
	ConnectTimeout time.Duration
}

// DefaultOptions returns sane pool defaults.
func DefaultOptions() Options {
	return Options{
		MaxOpenConns:    25,
		MaxIdleConns:    25,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
		LogLevel:        gormlogger.Warn,
		ConnectTimeout:  15 * time.Second,
	}
}

// New opens a PostgreSQL connection pool and verifies it with a ping.
func New(opts Options) (*gorm.DB, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, errors.New("database: empty dsn")
	}

	db, err := openDB(opts)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database: pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	ping := sqlDB.Ping
	if opts.ConnectTimeout > 0 {
		ping = func() error {
			ctx, cancel := context.WithTimeout(context.Background(), opts.ConnectTimeout)
			defer cancel()
			return sqlDB.PingContext(ctx)
		}
	}
	if err := ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return db, nil
}

// openDB runs gorm.Open under ConnectTimeout. gorm.Open dials eagerly
// (it pings before returning), so the timeout must wrap Open itself: a
// PingContext afterwards never runs against a blackhole DSN. On timeout
// the opener goroutine leaks until its dial fails; the boot fails with
// it, so the leak ends with the process.
func openDB(opts Options) (*gorm.DB, error) {
	type result struct {
		db  *gorm.DB
		err error
	}
	ch := make(chan result, 1)
	go func() {
		db, err := gorm.Open(postgres.New(postgres.Config{DSN: opts.DSN}), &gorm.Config{
			Logger: gormlogger.Default.LogMode(opts.LogLevel),
		})
		ch <- result{db: db, err: err}
	}()
	if opts.ConnectTimeout <= 0 {
		res := <-ch
		return res.db, res.err
	}
	timer := time.NewTimer(opts.ConnectTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.db, res.err
	case <-timer.C:
		return nil, fmt.Errorf("connect timeout after %s", opts.ConnectTimeout)
	}
}
