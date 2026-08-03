package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/config"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/grpcserver"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/mcpproxy"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/mcpserver"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/reconcile"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn(cfg))
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := db.NewStore(pool)
	k8sc, err := k8s.New(cfg.Namespace, cfg.E2eRunnerImage)
	if err != nil {
		slog.Error("k8s client init failed", "error", err)
		os.Exit(1)
	}
	proxy := mcpproxy.New(func(taskID string) string { return k8s.PlaywrightURLFor(cfg.Namespace, taskID) })

	mux := http.NewServeMux()
	mux.Handle("/mcp/", mcpserver.New(store, k8sc, proxy, cfg.E2eHost))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	grpcSrv := grpc.NewServer()
	agentfleetv1.RegisterE2EProvisionerServiceServer(grpcSrv, grpcserver.New(store))

	reconcileInterval, _ := strconv.Atoi(cfg.ReconcileInterval)
	loop := reconcile.New(store, k8sc, proxy)
	go func() {
		if err := loop.Run(ctx, time.Duration(reconcileInterval)*time.Millisecond); err != nil {
			slog.Error("reconcile loop exited", "error", err)
		}
	}()

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
		if err != nil {
			slog.Error("grpc listen failed", "error", err)
			return
		}
		slog.Info("e2e-provisioner grpc listening", "port", cfg.GRPCPort)
		if err := grpcSrv.Serve(lis); err != nil {
			slog.Error("grpc serve failed", "error", err)
		}
	}()

	go func() {
		slog.Info("e2e-provisioner http listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http serve failed", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcSrv.GracefulStop()
}

func dsn(cfg config.Config) string {
	return fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, cfg.DBPassword)
}
