package agent

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"

	api "github.com/wtran29/proglog/api/v1"
	"github.com/wtran29/proglog/internal/auth"
	"github.com/wtran29/proglog/internal/discovery"
	"github.com/wtran29/proglog/internal/log"
	"github.com/wtran29/proglog/internal/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Agent references each component (log, server, membership, replicator)
type Agent struct {
	Config
	log          *log.Log
	server       *grpc.Server
	membership   *discovery.Membership
	replicator   *log.Replicator
	shutdown     bool
	shutdowns    chan struct{}
	shutdownLock sync.Mutex
}

// Config comprises the components'
// params to pass them through to the components
type Config struct {
	ServerTLSConfig *tls.Config
	PeerTLSConfig   *tls.Config
	DataDir         string
	BindAddr        string
	RPCPort         int
	NodeName        string
	StartJoinAddrs  []string
	ACLModelFile    string
	ACLPolicyFile   string
}

// RPCAddr gets the RPC address host:port
func (c Config) RPCAddr() (string, error) {
	host, _, err := net.SplitHostPort(c.BindAddr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", host, c.RPCPort), nil
}

// New creates an Agent and runs a set of methods to set up and run agent's components
func New(config Config) (*Agent, error) {
	a := &Agent{
		Config:    config,
		shutdowns: make(chan struct{}),
	}
	setup := []func() error{
		a.setupLogger,
		a.setupLog,
		a.setupServer,
		a.setupMembership,
	}
	for _, fn := range setup {
		if err := fn(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// setupLogger sets up the logger
func (a *Agent) setupLogger() error {
	logger, err := zap.NewDevelopment()
	if err != nil {
		return err
	}
	zap.ReplaceGlobals(logger)
	return nil
}

// setupLog sets up the log
func (a *Agent) setupLog() error {
	var err error
	a.log, err = log.NewLog(a.Config.DataDir, log.Config{})
	return err
}

// setupServer sets up the rpc server
func (a *Agent) setupServer() error {
	authorizer := auth.New(a.Config.ACLModelFile, a.Config.ACLPolicyFile)
	serverConfig := &server.Config{
		CommitLog:  a.log,
		Authorizer: authorizer,
	}
	var opts []grpc.ServerOption
	if a.Config.ServerTLSConfig != nil {
		creds := credentials.NewTLS(a.Config.ServerTLSConfig)
		opts = append(opts, grpc.Creds(creds))
	}
	var err error
	a.server, err = server.NewGRPCServer(serverConfig, opts...)
	if err != nil {
		return err
	}
	rpcAddr, err := a.RPCAddr()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", rpcAddr)
	if err != nil {
		return err
	}
	go func() {
		if err := a.server.Serve(ln); err != nil {
			_ = a.Shutdown()
		}
	}()
	return err
}

// setupMembership sets up a Replicator with the gRPC dial options needed to connect to other servers
// and a client so the replicator can connect to other servers, consume their data, produce a copy of the
// data to the local server. Then Membership passing in the replicator and its handler to notify the
// replicator when servers join and leave the cluster
func (a *Agent) setupMembership() error {
	rpcAddr, err := a.Config.RPCAddr()
	if err != nil {
		return err
	}
	var opts []grpc.DialOption
	if a.Config.PeerTLSConfig != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(a.Config.PeerTLSConfig)))
	}
	conn, err := grpc.Dial(rpcAddr, opts...)
	if err != nil {
		return err
	}
	client := api.NewLogClient(conn)
	a.replicator = &log.Replicator{
		DialOptions: opts,
		LocalServer: client,
	}
	a.membership, err = discovery.New(a.replicator, discovery.Config{
		NodeName: a.Config.NodeName,
		BindAddr: a.Config.BindAddr,
		Tags: map[string]string{
			"rpc_addr": rpcAddr,
		},
		StartJoinAddrs: a.Config.StartJoinAddrs,
	})
	return err
}

// Shutdown shut downs the agent gracefully by leaving the membership so other servers will
// know it has left the cluster and does not continue to receive discovery events,
// closing the replicator so it does not continue to replicate, stops server from accepting new
// connections and blocks until all pending RPCs finish, and closing the log
func (a *Agent) Shutdown() error {
	a.shutdownLock.Lock()
	defer a.shutdownLock.Unlock()
	if a.shutdown {
		return nil
	}
	a.shutdown = true
	close(a.shutdowns)

	shutdown := []func() error{
		a.membership.Leave,
		a.replicator.Close,
		func() error {
			a.server.GracefulStop()
			return nil
		},
		a.log.Close,
	}
	for _, fn := range shutdown {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}
