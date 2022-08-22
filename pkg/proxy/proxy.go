// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"sync"

	"github.com/pingcap/TiProxy/pkg/config"
	mgrns "github.com/pingcap/TiProxy/pkg/manager/namespace"
	"github.com/pingcap/TiProxy/pkg/proxy/backend"
	"github.com/pingcap/TiProxy/pkg/proxy/client"
	"github.com/pingcap/TiProxy/pkg/util/errors"
	"github.com/pingcap/TiProxy/pkg/util/security"
	"github.com/pingcap/TiProxy/pkg/util/waitgroup"
	"github.com/pingcap/tidb/metrics"
	"go.uber.org/zap"
)

type serverState struct {
	sync.RWMutex
	tcpKeepAlive   bool
	connID         uint64
	maxConnections uint64
	clients        map[uint64]*client.ClientConnection
}

type SQLServer struct {
	listener         net.Listener
	logger           *zap.Logger
	nsmgr            *mgrns.NamespaceManager
	serverTLSConfig  *tls.Config
	clusterTLSConfig *tls.Config
	wg               waitgroup.WaitGroup

	mu serverState
}

// NewSQLServer creates a new SQLServer.
func NewSQLServer(logger *zap.Logger, workdir string, cfg config.ProxyServer, scfg config.Security, nsmgr *mgrns.NamespaceManager) (*SQLServer, error) {
	var err error

	s := &SQLServer{
		logger: logger,
		nsmgr:  nsmgr,
		mu: serverState{
			tcpKeepAlive:   cfg.TCPKeepAlive,
			maxConnections: cfg.MaxConnections,
			connID:         0,
			clients:        make(map[uint64]*client.ClientConnection),
		},
	}

	if s.serverTLSConfig, err = security.CreateServerTLSConfig(scfg.Server.CA, scfg.Server.Key, scfg.Server.Cert, scfg.RSAKeySize, workdir); err != nil {
		return nil, err
	}
	if s.clusterTLSConfig, err = security.CreateClientTLSConfig(scfg.Cluster.CA, scfg.Cluster.Key, scfg.Cluster.Cert); err != nil {
		return nil, err
	}

	s.listener, err = net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *SQLServer) Run(ctx context.Context) error {
	metrics.ServerEventCounter.WithLabelValues(metrics.EventStart).Inc()

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		default:
			conn, err := s.listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return nil
				}

				s.logger.Error("accept failed", zap.Error(err))
				return err
			}

			s.wg.Run(func() {
				s.onConn(ctx, conn)
			})
		}
	}
}

func (s *SQLServer) onConn(ctx context.Context, conn net.Conn) {
	s.mu.Lock()
	conns := uint64(len(s.mu.clients))
	maxConns := s.mu.maxConnections
	tcpKeepAlive := s.mu.tcpKeepAlive

	// 'maxConns == 0' => unlimited connections
	if maxConns != 0 && maxConns <= conns {
		s.mu.Unlock()
		s.logger.Warn("too many connections", zap.Uint64("max connections", maxConns), zap.String("addr", conn.RemoteAddr().Network()), zap.Error(conn.Close()))
		return
	}

	connID := s.mu.connID
	s.mu.connID++
	logger := s.logger.With(zap.Uint64("connID", connID))
	clientConn := client.NewClientConnection(logger, conn, s.serverTLSConfig, s.clusterTLSConfig, s.nsmgr, backend.NewBackendConnManager(connID))
	s.mu.clients[connID] = clientConn
	s.mu.Unlock()

	logger.Info("new connection", zap.String("remoteAddr", conn.RemoteAddr().String()))
	metrics.ConnGauge.Inc()

	defer func() {
		s.mu.Lock()
		delete(s.mu.clients, connID)
		s.mu.Unlock()

		logger.Info("connection closed", zap.Error(clientConn.Close()))
		metrics.ConnGauge.Dec()
	}()

	if tcpKeepAlive {
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			if err := tcpConn.SetKeepAlive(true); err != nil {
				logger.Warn("failed to set tcp keep alive option", zap.Error(err))
			}
		}
	}

	clientConn.Run(ctx)
}

// Close closes the server.
func (s *SQLServer) Close() error {
	var errs []error
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	s.mu.Lock()
	for _, conn := range s.mu.clients {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Unlock()

	s.wg.Wait()

	metrics.ServerEventCounter.WithLabelValues(metrics.EventClose).Inc()
	return errors.Collect(ErrCloseServer, errs...)
}