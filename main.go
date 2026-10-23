// Command fakegateway stands in for one Iranian payment provider at a time, so
// a local or staging end-to-end run needs nothing external. GATEWAY names the
// vendor this process plays; each serves its real paths at the root of its own
// host. It is a test double: state lives in memory, and nobody should ever
// point it at real money.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/47vigen/fakeGateway/core"
	"github.com/47vigen/fakeGateway/vendors/behpardakht"
	"github.com/47vigen/fakeGateway/vendors/digipay"
	"github.com/47vigen/fakeGateway/vendors/gsmpay"
	"github.com/47vigen/fakeGateway/vendors/kamanlend"
	"github.com/47vigen/fakeGateway/vendors/moneysa"
	"github.com/47vigen/fakeGateway/vendors/sep"
	"github.com/47vigen/fakeGateway/vendors/setareyek"
	"github.com/47vigen/fakeGateway/vendors/vibe"
	"github.com/47vigen/fakeGateway/vendors/zarinplus"
)

// vendors is the registry: a new vendor is one line here.
func vendors() []core.Vendor {
	return []core.Vendor{
		behpardakht.New(),
		digipay.New(),
		gsmpay.New(),
		kamanlend.New(),
		moneysa.New(),
		sep.New(),
		setareyek.New(),
		vibe.New(),
		zarinplus.New(),
	}
}

func pick(name string) (core.Vendor, error) {
	var known []string
	for _, v := range vendors() {
		if v.Name() == name {
			return v, nil
		}
		known = append(known, v.Name())
	}
	slices.Sort(known)
	if name == "" {
		return nil, fmt.Errorf("GATEWAY is required; known vendors: %s", strings.Join(known, ", "))
	}
	return nil, fmt.Errorf("unknown GATEWAY %q; known vendors: %s", name, strings.Join(known, ", "))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	v, err := pick(os.Getenv("GATEWAY"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakegateway:", err)
		os.Exit(2)
	}
	addr := env("LISTEN_ADDR", ":8090")
	base := strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8090"), "/")
	h, c := core.Handler(v, base, core.Behaviour{
		DefaultOutcome: env("DEFAULT_OUTCOME", core.OutcomeManual),
		EnforceAmounts: env("ENFORCE_AMOUNTS", "true") != "false",
	})

	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("fake gateway listening", "gateway", v.Name(), "addr", addr,
			"public_base_url", base, "behaviour", c.Behaviour())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}
