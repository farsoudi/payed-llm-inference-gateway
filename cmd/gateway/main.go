package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/farsoudi/payed-llm-inference/internal/auth"
	"github.com/farsoudi/payed-llm-inference/internal/config"
	"github.com/farsoudi/payed-llm-inference/internal/domain"
	"github.com/farsoudi/payed-llm-inference/internal/httpapi"
	"github.com/farsoudi/payed-llm-inference/internal/ledger"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "user":
		userCommand(os.Args[2:])
	case "topup":
		topupCommand(os.Args[2:])
	default:
		if os.Args[1][0] == '-' {
			serve(os.Args[1:])
			return
		}
		usage()
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to KEY=VALUE configuration file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*configPath)
	fatalIf(err)
	fatalIf(cfg.Validate())
	openCtx, openCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer openCancel()
	store, err := ledger.Open(openCtx, cfg.PostgresURL)
	fatalIf(err)
	defer store.Close()
	server := httpapi.New(cfg, store)
	slog.Info("gateway listening", "address", cfg.ListenAddr, "network", cfg.Network, "ollama", cfg.OllamaURL)
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalIf(err)
		}
	}()
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fatalIf(httpServer.Shutdown(ctx))
}

func userCommand(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		addUser(args[1:])
	case "list":
		listUsers(args[1:])
	case "revoke":
		revokeUser(args[1:])
	case "limit":
		limitUser(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func addUser(args []string) {
	fs := adminFlags("user add")
	label := fs.String("label", "", "human-readable label")
	rate := fs.Int("rate", 0, "requests per minute (defaults to config)")
	concurrency := fs.Int("concurrency", 0, "maximum concurrent requests (defaults to config)")
	_ = fs.Parse(args)
	cfg, store := adminStore(fs)
	defer store.Close()
	if *rate == 0 {
		*rate = cfg.DefaultRatePerMin
	}
	if *concurrency == 0 {
		*concurrency = cfg.DefaultConcurrency
	}
	if *rate <= 0 || *concurrency <= 0 {
		fatalIf(fmt.Errorf("rate and concurrency must be positive"))
	}
	key, err := auth.NewKey()
	fatalIf(err)
	ctx, cancel := adminContext()
	defer cancel()
	_, err = store.CreateUser(ctx, auth.Hash(key), *label, *rate, *concurrency)
	fatalIf(err)
	fmt.Println(key)
}

func listUsers(args []string) {
	fs := adminFlags("user list")
	_ = fs.Parse(args)
	_, store := adminStore(fs)
	defer store.Close()
	ctx, cancel := adminContext()
	defer cancel()
	users, err := store.ListUsers(ctx)
	fatalIf(err)
	_ = json.NewEncoder(os.Stdout).Encode(users)
}

func revokeUser(args []string) {
	fs := adminFlags("user revoke")
	key, keyFile, keyStdin := keyFlags(fs)
	_ = fs.Parse(args)
	_, store := adminStore(fs)
	defer store.Close()
	*key = resolveAdminKey(*key, *keyFile, *keyStdin)
	if *key == "" {
		fatalIf(fmt.Errorf("--key is required"))
	}
	ctx, cancel := adminContext()
	defer cancel()
	fatalIf(store.DeleteUser(ctx, auth.Hash(*key)))
}

func limitUser(args []string) {
	fs := adminFlags("user limit")
	key, keyFile, keyStdin := keyFlags(fs)
	rate := fs.Int("rate", 0, "requests per minute")
	concurrency := fs.Int("concurrency", 0, "maximum concurrent requests")
	_ = fs.Parse(args)
	_, store := adminStore(fs)
	defer store.Close()
	*key = resolveAdminKey(*key, *keyFile, *keyStdin)
	if *key == "" || *rate <= 0 || *concurrency <= 0 {
		fatalIf(fmt.Errorf("--key, positive --rate, and positive --concurrency are required"))
	}
	ctx, cancel := adminContext()
	defer cancel()
	fatalIf(store.SetLimits(ctx, auth.Hash(*key), *rate, *concurrency))
}

func topupCommand(args []string) {
	if len(args) == 0 || args[0] != "reconcile" {
		usage()
		os.Exit(2)
	}
	fs := adminFlags("topup reconcile")
	key, keyFile, keyStdin := keyFlags(fs)
	amount := fs.Int64("amount-micro", 0, "top-up amount in micro-USDC")
	tx := fs.String("transaction", "", "settlement transaction hash")
	payer := fs.String("payer", "", "payer address from settlement")
	network := fs.String("network", "", "settlement network")
	_ = fs.Parse(args[1:])
	_, store := adminStore(fs)
	defer store.Close()
	*key = resolveAdminKey(*key, *keyFile, *keyStdin)
	if *key == "" || *amount <= 0 || *tx == "" || *network == "" {
		fatalIf(fmt.Errorf("--key, positive --amount-micro, --transaction, and --network are required"))
	}
	ctx, cancel := adminContext()
	defer cancel()
	_, _, err := store.CreditTopUp(ctx, domain.TopUp{KeyHash: auth.Hash(*key), Amount: *amount, Transaction: *tx, Payer: *payer, Network: *network})
	fatalIf(err)
	fmt.Printf("reconciled %d micro-USDC on %s\n", *amount, *network)
}

func adminFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.String("config", "", "path to KEY=VALUE configuration file")
	return fs
}

func keyFlags(fs *flag.FlagSet) (key, keyFile *string, keyStdin *bool) {
	return fs.String("key", "", "API key"),
		fs.String("key-file", "", "read the API key from a protected file"),
		fs.Bool("key-stdin", false, "read the API key from stdin")
}

func adminStore(fs *flag.FlagSet) (config.Config, ledger.Store) {
	path := fs.Lookup("config").Value.String()
	cfg, err := config.Load(path)
	fatalIf(err)
	if cfg.PostgresURL == "" {
		fatalIf(fmt.Errorf("POSTGRES_URL is required"))
	}
	ctx, cancel := adminContext()
	defer cancel()
	store, err := ledger.Open(ctx, cfg.PostgresURL)
	fatalIf(err)
	return cfg, store
}

func adminContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func resolveAdminKey(value, path string, stdin bool) string {
	if path != "" && stdin {
		fatalIf(fmt.Errorf("use only one of --key-file and --key-stdin"))
	}
	if path != "" {
		data, err := os.ReadFile(path)
		fatalIf(err)
		return strings.TrimSpace(string(data))
	}
	if stdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		fatalIf(err)
		return strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(value)
}

func fatalIf(err error) {
	if err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, "usage: gateway serve --config FILE | gateway user {add|list|revoke|limit} --config FILE | gateway topup reconcile ...")
}
