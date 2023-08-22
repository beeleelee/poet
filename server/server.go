package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	grpcmw "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"

	"github.com/spacemeshos/poet/config"
	"github.com/spacemeshos/poet/logging"
	api "github.com/spacemeshos/poet/release/proto/go/rpc/api/v1"
	"github.com/spacemeshos/poet/rpc"
	"github.com/spacemeshos/poet/service"
)

type Server struct {
	svc          *service.Service
	cfg          config.Config
	rpcListener  net.Listener
	restListener net.Listener
}

func New(ctx context.Context, cfg config.Config) (*Server, error) {
	// Resolve the RPC listener
	addr, err := net.ResolveTCPAddr("tcp", cfg.RawRPCListener)
	if err != nil {
		return nil, err
	}
	rpcListener, err := net.Listen(addr.Network(), addr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %v", err)
	}

	// Resolve the REST listener
	addr, err = net.ResolveTCPAddr("tcp", cfg.RawRESTListener)
	if err != nil {
		return nil, err
	}

	restListener, err := net.Listen(addr.Network(), addr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %v", err)
	}

	if _, err := os.Stat(cfg.DataDir); os.IsNotExist(err) {
		if err := os.Mkdir(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
	}

	svc, err := service.NewService(ctx, cfg.Service, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create Service: %v", err)
	}

	return &Server{
		svc:          svc,
		cfg:          cfg,
		rpcListener:  rpcListener,
		restListener: restListener,
	}, nil
}

func (s *Server) Close() error {
	err := s.rpcListener.Close()
	return errors.Join(err, s.restListener.Close())
}

// GrpcAddr returns the address that server is listening on for GRPC.
func (s *Server) GrpcAddr() net.Addr {
	return s.rpcListener.Addr()
}

// GrpcRestProxyAddr returns the address that REST-GRPC proxy is listening on.
func (s *Server) GrpcRestProxyAddr() net.Addr {
	return s.restListener.Addr()
}

// Start starts the RPC server.
func (s *Server) Start(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	serverGroup, ctx := errgroup.WithContext(ctx)

	logger := logging.FromContext(ctx)

	// Initialize and register the implementation of gRPC interface
	var grpcServer *grpc.Server
	var proxyRegstr []func(context.Context, *proxy.ServeMux, string, []grpc.DialOption) error
	options := []grpc.ServerOption{
		grpc.UnaryInterceptor(grpcmw.ChainUnaryServer(
			loggerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
		)),
		// XXX: this is done to prevent routers from cleaning up our connections (e.g aws load balances..)
		// TODO: these parameters work for now but we might need to revisit or add them as configuration
		// TODO: Configure maxconns, maxconcurrentcons ..
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     time.Minute * 120,
			MaxConnectionAge:      time.Minute * 180,
			MaxConnectionAgeGrace: time.Minute * 10,
			Time:                  time.Minute,
			Timeout:               time.Minute * 3,
		}),
	}

	proofsDbPath := filepath.Join(s.cfg.DataDir, "proofs")
	proofsDb, err := service.NewProofsDatabase(proofsDbPath, s.svc.ProofsChan())
	if err != nil {
		return fmt.Errorf("failed to create proofs DB: %w", err)
	}
	serverGroup.Go(func() error {
		return proofsDb.Run(ctx)
	})

	serverGroup.Go(func() error {
		return s.svc.Run(ctx)
	})

	if err := s.svc.Start(ctx); err != nil {
		stop()
		return fmt.Errorf("service stopped with error: %v (serverGroup: %v)", err, serverGroup.Wait())
	}

	rpcServer := rpc.NewServer(s.svc, proofsDb, s.cfg)
	grpcServer = grpc.NewServer(options...)

	api.RegisterPoetServiceServer(grpcServer, rpcServer)
	proxyRegstr = append(proxyRegstr, api.RegisterPoetServiceHandlerFromEndpoint)

	reflection.Register(grpcServer)
	grpc_prometheus.Register(grpcServer)

	// Start the gRPC server listening for HTTP/2 connections.
	serverGroup.Go(func() error {
		logger.Sugar().Infof("GRPC server listening on %s", s.rpcListener.Addr())
		return grpcServer.Serve(s.rpcListener)
	})

	// Start the REST proxy for the gRPC server above.
	mux := proxy.NewServeMux()
	for _, r := range proxyRegstr {
		err := r(
			ctx,
			mux,
			s.rpcListener.Addr().String(),
			[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		)
		if err != nil {
			return err
		}
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second * 5}
	serverGroup.Go(func() error {
		logger.Sugar().Infof("REST proxy starts listening on %s", s.restListener.Addr())
		err := server.Serve(s.restListener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})

	// Wait for the server to shut down gracefully
	<-ctx.Done()
	grpcServer.GracefulStop()
	err = server.Shutdown(context.Background())
	return errors.Join(err, serverGroup.Wait())
}

// loggerInterceptor returns UnaryServerInterceptor handler to log all RPC server incoming requests.
func loggerInterceptor(
	logger *zap.Logger,
) func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		peer, _ := peer.FromContext(ctx)

		logger := logger.Named(info.FullMethod).With(zap.Stringer("request_id", uuid.New()))
		ctx = logging.NewContext(ctx, logger)

		if msg, ok := req.(fmt.Stringer); ok {
			logger.Debug("new GRPC", zap.Stringer("from", peer.Addr), zap.Stringer("message", msg))
		}

		resp, err := handler(ctx, req)
		if err != nil {
			logger.Info("FAILURE", zap.Error(err))
		}
		return resp, err
	}
}
