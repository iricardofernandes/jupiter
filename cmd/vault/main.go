package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/kms"
	"github.com/iricardofernandes/jupiter/internal/vault/server"
)

const usage = `usage: vault [command]

With no command, serves the vault: its internal interface over mTLS on JUPITER_HTTP_ADDR
(default :8082), and the tokenization route for web pages on JUPITER_VAULT_PUBLIC_ADDR
(default :8083).

commands:
  migrate   apply the vault's database migrations
  rewrap    move every data key to the active key-encryption key
  keys      count the cards under each key-encryption key

environment:
  JUPITER_DATABASE_URL           the vault's own database
  JUPITER_VAULT_KEKS             key-encryption keys, as id:base64,id:base64
  JUPITER_VAULT_ACTIVE_KEK       the id of the key new cards are wrapped under
  JUPITER_VAULT_FINGERPRINT_KEY  32 random bytes in base64, never rotated
  JUPITER_VAULT_TLS_CERT, JUPITER_VAULT_TLS_KEY, JUPITER_VAULT_CLIENT_CA
                                 the server's certificate and the CA of its clients`

const (
	purgeInterval  = time.Minute
	rewrapInterval = 30 * time.Second
	rewrapBatch    = 500
)

func main() {
	if len(os.Args) > 1 {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		err := command(ctx, os.Args[1:])
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "vault:", err)
			os.Exit(1)
		}
		return
	}
	service.Main("vault", ":8082", build)
}

var errUsage = errors.New(usage)

func command(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	cfg, err := service.ConfigFromEnv("vault", ":8082", os.Getenv)
	if err != nil {
		return err
	}
	if args[0] == "migrate" {
		return migrate(ctx, cfg)
	}
	svc, closeFn, err := newService(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer closeFn()
	switch args[0] {
	case "rewrap":
		n, err := svc.RewrapAll(ctx, rewrapBatch)
		fmt.Printf("rewrapped %d data keys\n", n)
		return err
	case "keys":
		usage, err := svc.KeyUsage(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(usage)
	default:
		return errUsage
	}
}

func migrate(ctx context.Context, cfg service.Config) error {
	if cfg.DatabaseURL == "" {
		return errors.New("JUPITER_DATABASE_URL is required")
	}
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return server.Migrate(ctx, pool)
}

func newService(ctx context.Context, cfg service.Config, logger *slog.Logger) (*server.Service, func(), error) {
	if cfg.DatabaseURL == "" {
		return nil, nil, errors.New("JUPITER_DATABASE_URL is required")
	}
	keys, err := newKMS()
	if err != nil {
		return nil, nil, err
	}
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, postgres.ServeTimeouts)
	if err != nil {
		return nil, nil, err
	}
	if err := server.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return server.New(server.Config{Pool: pool, KMS: keys, Logger: logger}), pool.Close, nil
}

func newKMS() (*kms.Local, error) {
	keyring, err := kms.ParseKeyring(os.Getenv("JUPITER_VAULT_KEKS"))
	if err != nil {
		return nil, fmt.Errorf("JUPITER_VAULT_KEKS: %w", err)
	}
	fingerprintKey, err := base64.StdEncoding.DecodeString(os.Getenv("JUPITER_VAULT_FINGERPRINT_KEY"))
	if err != nil {
		return nil, fmt.Errorf("JUPITER_VAULT_FINGERPRINT_KEY: %w", err)
	}
	return kms.NewLocal(keyring, os.Getenv("JUPITER_VAULT_ACTIVE_KEK"), fingerprintKey)
}

func build(ctx context.Context, cfg service.Config, logger *slog.Logger) (service.App, error) {
	tlsConfig, err := mtls.LoadServerConfig(os.Getenv("JUPITER_VAULT_TLS_CERT"), os.Getenv("JUPITER_VAULT_TLS_KEY"),
		os.Getenv("JUPITER_VAULT_CLIENT_CA"), vault.APIIdentity, vault.WorkerIdentity)
	if err != nil {
		return service.App{}, err
	}
	publicAddr := os.Getenv("JUPITER_VAULT_PUBLIC_ADDR")
	if publicAddr == "" {
		publicAddr = ":8083"
	}
	svc, closeFn, err := newService(ctx, cfg, logger)
	if err != nil {
		return service.App{}, err
	}
	return service.App{
		Handler: svc.InternalHandler(),
		TLS:     tlsConfig,
		Background: []func(context.Context) error{
			servePublic(publicAddr, svc, logger),
			service.Every(logger, "purge_unclaimed", purgeInterval, func(ctx context.Context) error {
				_, err := svc.PurgeUnclaimed(ctx)
				return err
			}),
			// Rewrapping runs all the time: after a new key is activated it moves the data
			// keys over, and otherwise it finds nothing to do.
			service.Every(logger, "rewrap", rewrapInterval, func(ctx context.Context) error {
				_, err := svc.RewrapAll(ctx, rewrapBatch)
				return err
			}),
		},
		Close: closeFn,
	}, nil
}

// servePublic serves the web-page route beside the internal listener. Behind a real
// deployment it sits behind the edge, with its own rate limits.
func servePublic(addr string, svc *server.Service, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("listening on %s: %w", addr, err)
		}
		return service.Serve(ctx, ln, service.App{Handler: svc.PublicHandler()}, logger.With("listener", "public"))
	}
}
