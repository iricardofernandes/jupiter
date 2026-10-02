package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/ratelimit"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/kms"
	"github.com/iricardofernandes/jupiter/internal/vault/server"
)

const usage = `usage: vault [command]

With no command, serves the vault: its internal interface over mTLS on JUPITER_HTTP_ADDR
(default :8082), and the tokenization route for web pages on JUPITER_VAULT_PUBLIC_ADDR
(default 127.0.0.1:8083).

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
                                 the server's certificate and the CA of its clients
  JUPITER_VAULT_PUBLIC_TLS_CERT, JUPITER_VAULT_PUBLIC_TLS_KEY
                                 the public route's certificate; without one, it serves
                                 plain HTTP only on a loopback address, or anywhere when
                                 JUPITER_VAULT_PUBLIC_BEHIND_EDGE=true says an edge ends TLS
  JUPITER_VAULT_PUBLIC_RATE, JUPITER_VAULT_PUBLIC_BURST
                                 cards a second, and at once, from one address (2, 20)
  JUPITER_TRUSTED_PROXIES        proxies whose X-Forwarded-For names the client`

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
	limits, err := publicLimits(os.Getenv)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	limits.Pool, limits.KMS, limits.Logger = pool, keys, logger
	return server.New(limits), pool.Close, nil
}

// publicLimits reads how fast one address may make tokens on the web-page route, and
// which proxies name the address.
func publicLimits(getenv func(string) string) (server.Config, error) {
	var cfg server.Config
	if v := getenv("JUPITER_VAULT_PUBLIC_RATE"); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil || rate <= 0 {
			return cfg, fmt.Errorf("JUPITER_VAULT_PUBLIC_RATE must be a positive number of cards a second: %q", v)
		}
		cfg.PublicRate = rate
	}
	if v := getenv("JUPITER_VAULT_PUBLIC_BURST"); v != "" {
		burst, err := strconv.Atoi(v)
		if err != nil || burst <= 0 {
			return cfg, fmt.Errorf("JUPITER_VAULT_PUBLIC_BURST must be a positive number of cards: %q", v)
		}
		cfg.PublicBurst = burst
	}
	trusted, err := ratelimit.ParseTrusted(getenv("JUPITER_TRUSTED_PROXIES"))
	cfg.Clients.Trusted = trusted
	return cfg, err
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
	public, err := publicListener(os.Getenv)
	if err != nil {
		return service.App{}, err
	}
	svc, closeFn, err := newService(ctx, cfg, logger)
	if err != nil {
		return service.App{}, err
	}
	return service.App{
		Handler: svc.InternalHandler(),
		TLS:     tlsConfig,
		Background: []func(context.Context) error{
			servePublic(public, svc, logger),
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

// public is where the web-page route listens, and its TLS if it ends TLS itself.
type public struct {
	addr string
	tls  *tls.Config
}

// publicListener reads where the web-page route listens. Cards cross it, so it serves
// plain HTTP only on a loopback address, or behind an edge that ends TLS.
func publicListener(getenv func(string) string) (public, error) {
	p := public{addr: getenv("JUPITER_VAULT_PUBLIC_ADDR")}
	if p.addr == "" {
		p.addr = "127.0.0.1:8083"
	}
	if cert, key := getenv("JUPITER_VAULT_PUBLIC_TLS_CERT"), getenv("JUPITER_VAULT_PUBLIC_TLS_KEY"); cert != "" || key != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return public{}, fmt.Errorf("JUPITER_VAULT_PUBLIC_TLS_CERT: %w", err)
		}
		p.tls = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		return p, nil
	}
	host, _, err := net.SplitHostPort(p.addr)
	if err != nil {
		return public{}, fmt.Errorf("JUPITER_VAULT_PUBLIC_ADDR: %w", err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && getenv("JUPITER_VAULT_PUBLIC_BEHIND_EDGE") != "true" {
		return public{}, fmt.Errorf("JUPITER_VAULT_PUBLIC_ADDR %s is not a loopback address: give the route a certificate, "+
			"or set JUPITER_VAULT_PUBLIC_BEHIND_EDGE=true when an edge in front of it ends TLS", p.addr)
	}
	return p, nil
}

// servePublic serves the web-page route beside the internal listener.
func servePublic(p public, svc *server.Service, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", p.addr)
		if err != nil {
			return fmt.Errorf("listening on %s: %w", p.addr, err)
		}
		return service.Serve(ctx, ln, service.App{Handler: svc.PublicHandler(), TLS: p.tls}, logger.With("listener", "public"))
	}
}
