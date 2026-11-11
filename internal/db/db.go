package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNoPassword  = errors.New("postgres password is empty")
	ErrBadPassword = errors.New("the app database password must be 20 or more letters and digits")
)

type Config struct {
	Host     string
	Port     int
	User     string
	Database string
	Password string
	SSLMode  string
}

func Open(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	if c.Password == "" {
		return nil, ErrNoPassword
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(c.User),
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     "/" + c.Database,
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	cfg.ConnConfig.Password = c.Password
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach postgres at %s: %w", u.Host, err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("make migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, files, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func SetAppPassword(ctx context.Context, pool *pgxpool.Pool, password string) error {
	if len(password) < 20 || strings.IndexFunc(password, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) >= 0 {
		return ErrBadPassword
	}
	if _, err := pool.Exec(ctx, "ALTER ROLE stackwright_app WITH LOGIN PASSWORD '"+password+"'"); err != nil {
		return fmt.Errorf("set the stackwright_app password: %w", err)
	}
	return nil
}
