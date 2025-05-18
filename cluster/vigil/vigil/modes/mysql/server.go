/*
 * Copyright Octelium Labs, LLC. All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License version 3,
 * as published by the Free Software Foundation of the License.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package mysql

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"sync"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/octelium/octelium/apis/cluster/coctovigilv1"
	"github.com/octelium/octelium/apis/main/corev1"
	"github.com/octelium/octelium/apis/rsc/rmetav1"
	"github.com/octelium/octelium/cluster/common/ocrypto"
	"github.com/octelium/octelium/cluster/common/octeliumc"
	"github.com/octelium/octelium/cluster/common/otelutils"
	"github.com/octelium/octelium/cluster/common/vutils"
	"github.com/octelium/octelium/cluster/vigil/vigil/controllers"
	"github.com/octelium/octelium/cluster/vigil/vigil/loadbalancer"
	"github.com/octelium/octelium/cluster/vigil/vigil/logentry"
	"github.com/octelium/octelium/cluster/vigil/vigil/metricutils"
	"github.com/octelium/octelium/cluster/vigil/vigil/modes"
	"github.com/octelium/octelium/cluster/vigil/vigil/octovigilc"
	"github.com/octelium/octelium/cluster/vigil/vigil/secretman"
	"github.com/octelium/octelium/cluster/vigil/vigil/vcache"
	"github.com/octelium/octelium/cluster/vigil/vigil/vigilutils"
	"github.com/octelium/octelium/pkg/apiutils/ucorev1"
	"github.com/octelium/octelium/pkg/grpcerr"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

type Server struct {
	octovigilC *octovigilc.Client
	vCache     *vcache.Cache

	lis net.Listener

	octeliumC octeliumc.ClientInterface

	cancelFn     context.CancelFunc
	doneComplete chan struct{}
	dctxMap      struct {
		mu      sync.Mutex
		dctxMap map[string]*dctx
	}

	mu       sync.Mutex
	isClosed bool

	svcCtl     *controllers.ServiceController
	sessionCtl *controllers.SessionController
	lbManager  *loadbalancer.LBManager

	secretMan *secretman.SecretManager

	crtMan struct {
		mu  sync.RWMutex
		crt *corev1.Secret
	}
	tlsCfgMan struct {
		tlsCfg *tls.Config
		mu     sync.RWMutex
	}
	metricsStore *metricsStore
}

type metricsStore struct {
	*metricutils.CommonMetrics
}

func (s *Server) svc() *corev1.Service {
	return s.vCache.GetService()
}

func (s *Server) SetClusterCertificate(crt *corev1.Secret) error {
	s.crtMan.mu.Lock()
	defer s.crtMan.mu.Unlock()
	s.crtMan.crt = crt
	return nil
}

func New(ctx context.Context, opts *modes.Opts) (*Server, error) {

	server := &Server{
		doneComplete: make(chan struct{}),
		vCache:       opts.VCache,
		octovigilC:   opts.OctovigilC,
		octeliumC:    opts.OcteliumC,
		lbManager:    opts.LBManager,
		svcCtl:       &controllers.ServiceController{},
		sessionCtl:   &controllers.SessionController{},
		secretMan:    opts.SecretMan,
		metricsStore: &metricsStore{},
	}

	server.dctxMap.dctxMap = make(map[string]*dctx)
	var err error
	server.metricsStore.CommonMetrics, err = metricutils.NewCommonMetrics(ctx, opts.VCache.GetService())
	if err != nil {
		return nil, err
	}

	return server, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return nil
	}

	s.isClosed = true
	s.cancelFn()

	zap.S().Debugf("Closing MySQL server")
	s.dctxMap.mu.Lock()
	for _, dctx := range s.dctxMap.dctxMap {
		dctx.close()
	}
	s.dctxMap.mu.Unlock()

	if s.lis != nil {
		s.lis.Close()
	}

	zap.S().Debugf("MySQL server closed")
	close(s.doneComplete)

	return nil
}

func (s *Server) handleConn(ctx context.Context, c net.Conn) {
	zap.S().Debugf("Started handling a new conn for: %s", c.RemoteAddr().String())

	startTime := time.Now()
	svc := s.svc()
	if svc == nil {
		zap.S().Warnf("Could not get the Service from cache")
		c.Close()
		return
	}

	authResp, err := s.octovigilC.AuthenticateAndAuthorize(ctx, &octovigilc.AuthenticateAndAuthorizeRequest{
		Request: s.getDownstreamReq(ctx, c),
	})
	if err != nil {
		zap.S().Debugf("Could not auth conn: %+v", err)
		c.Close()
		return
	}

	if authResp.IsAuthenticated && !authResp.IsAuthorized {
		logE := logentry.InitializeLogEntry(&logentry.InitializeLogEntryOpts{
			StartTime:       startTime,
			IsAuthenticated: true,
			ReqCtx:          authResp.RequestContext,
			Reason:          authResp.AuthorizationDecisionReason,
		})
		logE.Entry.Info.Type = &corev1.AccessLog_Entry_Info_Mysql{
			Mysql: &corev1.AccessLog_Entry_Info_MySQL{
				Type: corev1.AccessLog_Entry_Info_MySQL_SESSION_START,
			},
		}
		otelutils.EmitAccessLog(logE)
		c.Close()
		return
	}

	i := authResp.RequestContext

	zap.L().Debug("Creating new dctx", zap.Any("requestCtx", i))

	downstreamConn, err := s.getDownstreamConn(c)
	if err != nil {
		c.Close()
		return
	}

	zap.L().Debug("Got downstream conn", zap.Int("seq", int(downstreamConn.Sequence)))

	dctx := newDctx(ctx, c, i, s.secretMan, downstreamConn, authResp)
	if err := dctx.connect(ctx, s.lbManager, svc, s.secretMan); err != nil {
		zap.L().Error("Could not connect", zap.Error(err), zap.String("id", dctx.id))
		downstreamConn.Close()
		c.Close()
		return
	}

	{
		logE := logentry.InitializeLogEntry(&logentry.InitializeLogEntryOpts{
			StartTime:       startTime,
			IsAuthenticated: true,
			IsAuthorized:    true,
			ReqCtx:          i,
			ConnectionID:    dctx.id,
			Reason:          authResp.AuthorizationDecisionReason,
		})
		logE.Entry.Info.Type = &corev1.AccessLog_Entry_Info_Mysql{
			Mysql: &corev1.AccessLog_Entry_Info_MySQL{
				Type: corev1.AccessLog_Entry_Info_MySQL_SESSION_START,
			},
		}
		otelutils.EmitAccessLog(logE)
	}

	{
		s.dctxMap.mu.Lock()
		s.dctxMap.dctxMap[dctx.id] = dctx
		s.dctxMap.mu.Unlock()
	}

	s.metricsStore.AtRequestStart()
	dctx.serve(ctx)
	s.metricsStore.AtRequestEnd(dctx.createdAt, nil)

	defer dctx.close()

	{
		s.dctxMap.mu.Lock()
		delete(s.dctxMap.dctxMap, dctx.id)
		s.dctxMap.mu.Unlock()
	}

	{
		logE := logentry.InitializeLogEntry(&logentry.InitializeLogEntryOpts{
			StartTime:       startTime,
			IsAuthenticated: true,
			IsAuthorized:    true,
			ReqCtx:          i,
			ConnectionID:    dctx.id,
		})
		logE.Entry.Info.Type = &corev1.AccessLog_Entry_Info_Mysql{
			Mysql: &corev1.AccessLog_Entry_Info_MySQL{
				Type: corev1.AccessLog_Entry_Info_MySQL_SESSION_END,
			},
		}

		otelutils.EmitAccessLog(logE)
	}

}

func (s *Server) getDownstreamConn(c net.Conn) (*server.Conn, error) {
	s.tlsCfgMan.mu.RLock()
	defer s.tlsCfgMan.mu.RUnlock()
	zap.L().Debug("Getting downstream conn")

	mysqlSrv := server.NewServer(serverVersion,
		mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, s.tlsCfgMan.tlsCfg)

	return server.NewCustomizedConn(c, mysqlSrv, &credProvider{}, server.EmptyHandler{})
}

func (s *Server) getDownstreamReq(ctx context.Context, c net.Conn) *coctovigilv1.DownstreamRequest {

	return &coctovigilv1.DownstreamRequest{
		Source: vigilutils.GetDownstreamRequestSource(c),
	}
}

func (s *Server) setTLSConfig(ctx context.Context) error {

	crt, err := s.octeliumC.CoreC().GetSecret(ctx, &rmetav1.GetOptions{Name: vutils.ClusterCertSecretName})
	if err != nil && !grpcerr.IsNotFound(err) {
		return err
	}

	s.crtMan.mu.Lock()
	s.crtMan.crt = crt
	s.crtMan.mu.Unlock()

	s.tlsCfgMan.mu.Lock()
	s.tlsCfgMan.tlsCfg = &tls.Config{
		ClientAuth: tls.NoClientCert,
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			s.crtMan.mu.RLock()
			defer s.crtMan.mu.RUnlock()
			if s.crtMan.crt == nil {
				return nil, nil
			}
			return ocrypto.GetTLSCertificate(s.crtMan.crt)
		},
	}
	s.tlsCfgMan.mu.Unlock()

	return nil
}

func (s *Server) Run(ctx context.Context) error {

	zap.L().Debug("Starting running MySQL server")
	var err error
	s.lis, err = net.Listen("tcp", fmt.Sprintf(":%d", ucorev1.ToService(s.svc()).RealPort()))
	if err != nil {
		return err
	}

	svc := s.svc()

	if svc.Spec.IsTLS {
		if err := s.setTLSConfig(ctx); err != nil {
			return err
		}
	}

	ctx, cancelFn := context.WithCancel(ctx)
	s.cancelFn = cancelFn

	go s.serve(ctx)

	zap.L().Debug("MySQL server is now running")

	return nil
}

func (s *Server) serve(ctx context.Context) {
	zap.S().Debugf("Starting serving connections")

	for {
		conn, err := s.lis.Accept()

		if err != nil {
			zap.S().Debugf("Could not accept conn: %+v", err)
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				zap.S().Debugf("Timeout err")
				time.Sleep(100 * time.Millisecond)
				continue
			}

			select {
			case <-ctx.Done():
				zap.S().Debugf("shutting down server")
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}

		}

		go s.handleConn(ctx, conn)
	}
}

type credProvider struct{}

func (p *credProvider) CheckUsername(arg string) (bool, error) {
	return true, nil
}
func (p *credProvider) GetCredential(arg string) (string, bool, error) {
	return "", true, nil
}

/*
func setKeepAlive(conn net.Conn) error {
	tcpConn := conn.(*net.TCPConn)
	if err := tcpConn.SetKeepAlive(true); err != nil {
		return err
	}
	if err := tcpConn.SetKeepAlivePeriod(40 * time.Second); err != nil {
		return err
	}

	return nil
}
*/
