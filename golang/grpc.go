package gubernator

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mailgun/holster"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRequestSize = 1 * 1024 * 1024 // 1Mb
	maxBatchSize   = 1000
	Healthy        = "healthy"
	UnHealthy      = "unhealthy"
)

type Instance struct {
	health    HealthCheckResp
	wg        holster.WaitGroup
	log       *logrus.Entry
	conf      Config
	listener  net.Listener
	server    *grpc.Server
	peerMutex sync.RWMutex
	client    *PeerClient
}

// New creates a new gubernator instance.
func New(conf Config) (*Instance, error) {

	if err := ApplyConfigDefaults(&conf); err != nil {
		return nil, err
	}

	// TODO: Consider using golang.org/x/net/netutil
	//  netutil.LimitListener(listener,connectionLimit)

	listener, err := net.Listen("tcp", conf.ListenAddress)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to listen on %s", conf.ListenAddress)
	}

	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxRequestSize),
		grpc.StatsHandler(conf.Metrics.GRPCStatsHandler()))

	s := Instance{
		log:      logrus.WithField("category", "grpc"),
		listener: listener,
		server:   server,
		conf:     conf,
	}

	// Register our server with GRPC
	RegisterRateLimitServiceV1Server(server, &s)
	RegisterPeersServiceV1Server(server, &s)

	// Register our peer update callback
	s.conf.PeerSyncer.RegisterOnUpdate(s.updatePeers)

	// Register cache stats with out metrics collector
	s.conf.Metrics.RegisterCacheStats(s.conf.Cache)

	// Advertise address is our listen address if not specified
	holster.SetDefault(&s.conf.AdvertiseAddress, s.Address())

	return &s, nil
}

// Runs the gRPC server; returns when the server starts
func (s *Instance) Start() error {
	// Start the cache
	if err := s.conf.Cache.Start(); err != nil {
		return errors.Wrap(err, "failed to start cache")
	}

	// Start the metrics collector
	if err := s.conf.Metrics.Start(); err != nil {
		return errors.Wrap(err, "failed to start metrics collector")
	}

	// Start the GRPC server
	errs := make(chan error, 1)
	s.wg.Go(func() {
		// Serve will return a non-nil error unless Stop or GracefulStop is called.
		errs <- s.server.Serve(s.listener)
	})

	// Ensure the server is running before we return
	go func() {
		errs <- retry(2, time.Millisecond*500, func() error {
			conn, err := grpc.Dial(s.listener.Addr().String(), grpc.WithInsecure())
			if err != nil {
				return err
			}
			client := NewRateLimitServiceV1Client(conn)
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
			defer cancel()
			_, err = client.HealthCheck(ctx, &HealthCheckReq{})
			return err
		})
	}()

	// Wait until the server starts or errors
	err := <-errs
	if err != nil {
		return errors.Wrap(err, "while waiting for server to pass health check")
	}

	// Now that our service is up, register our server
	if err := s.conf.PeerSyncer.Start(s.conf.AdvertiseAddress); err != nil {
		return errors.Wrap(err, "failed to sync configs with other peers")
	}

	s.log.Infof("Gubernator Listening on %s ...", s.Address())
	return nil
}

func (s *Instance) Stop() {
	s.server.Stop()
	s.conf.PeerSyncer.Stop()
	s.conf.Metrics.Stop()
	s.wg.Wait()
}

// Return the address the server is listening too
func (s *Instance) Address() string {
	return s.listener.Addr().String()
}

func (s *Instance) GetRateLimits(ctx context.Context, r *Requests) (*RateLimits, error) {
	var resp RateLimits

	if len(r.Requests) > maxBatchSize {
		return nil, status.Errorf(codes.OutOfRange,
			"Requests.RateLimits list too large; max size is '%d'", maxBatchSize)
	}

	// TODO: Support getting multiple keys in an async manner (FanOut)
	for _, req := range r.Requests {
		globalKey := req.Namespace + "_" + req.UniqueKey
		var rl *RateLimit
		var peer *PeerClient
		var err error

		if len(req.UniqueKey) == 0 {
			rl = &RateLimit{Error: "field 'unique_key' cannot be empty"}
			goto NextRateLimit
		}

		if len(req.Namespace) == 0 {
			rl = &RateLimit{Error: "field 'namespace' cannot be empty"}
			goto NextRateLimit
		}

		s.peerMutex.RLock()
		peer, err = s.conf.Picker.Get(globalKey)
		if err != nil {
			s.peerMutex.RUnlock()
			rl = &RateLimit{
				Error: fmt.Sprintf("while finding peer that owns rate limit '%s' - '%s'", globalKey, err),
			}
			goto NextRateLimit
		}
		s.peerMutex.RUnlock()

		// If our server instance is the owner of this rate limit
		if peer.isOwner {
			// Apply our rate limit algorithm to the request
			rl, err = s.getRateLimit(req)
			if err != nil {
				rl = &RateLimit{
					Error: fmt.Sprintf("while applying rate limit for '%s' - '%s'", globalKey, err),
				}
				goto NextRateLimit
			}
		} else {
			// Make an RPC call to the peer that owns this rate limit
			rl, err = peer.GetPeerRateLimit(ctx, req)
			if err != nil {
				rl = &RateLimit{
					Error: fmt.Sprintf("while fetching rate limit '%s' from peer - '%s'", globalKey, err),
				}
			}

			// Inform the client of the owner key of the key
			rl.Metadata = map[string]string{"owner": peer.host}
		}
	NextRateLimit:
		resp.RateLimits = append(resp.RateLimits, rl)
	}
	return &resp, nil
}

func (s *Instance) UpdatePeerGlobals(ctx context.Context, r *UpdatePeerGlobalsReq) (*UpdatePeerGlobalsResp, error) {
	// NOT IMPLEMENTED
	return nil, nil
}

func (s *Instance) GetPeerRateLimits(ctx context.Context, r *GetPeerRateLimitsReq) (*GetPeerRateLimitsResp, error) {
	var resp GetPeerRateLimitsResp

	if len(r.Requests) > maxBatchSize {
		return nil, status.Errorf(codes.OutOfRange,
			"'PeerRequest.rate_limits' list too large; max size is '%d'", maxBatchSize)
	}

	for _, req := range r.Requests {
		rl, err := s.getRateLimit(req)
		if err != nil {
			// Return the error for this request
			rl = &RateLimit{Error: err.Error()}
		}
		resp.RateLimits = append(resp.RateLimits, rl)
	}
	return &resp, nil
}

// Returns the health of the peer.
func (s *Instance) HealthCheck(ctx context.Context, r *HealthCheckReq) (*HealthCheckResp, error) {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	return &s.health, nil
}

func (s *Instance) getRateLimit(r *Request) (*RateLimit, error) {
	s.conf.Cache.Lock()
	defer s.conf.Cache.Unlock()

	switch r.Algorithm {
	case Algorithm_TOKEN_BUCKET:
		return tokenBucket(s.conf.Cache, r)
	case Algorithm_LEAKY_BUCKET:
		return leakyBucket(s.conf.Cache, r)
	}
	return nil, errors.Errorf("invalid rate limit algorithm '%d'", r.Algorithm)
}

// Called by PeerSyncer when the cluster config changes
func (s *Instance) updatePeers(conf *PeerConfig) {
	picker := s.conf.Picker.New()
	var errs []string

	for _, peer := range conf.Peers {
		peerInfo, err := NewPeerClient(s.conf.Behaviors, peer)
		if err != nil {
			errs = append(errs, fmt.Sprintf("failed to connect to peer '%s'; consistent hash is incomplete", peer))
			continue
		}

		if info := s.conf.Picker.GetPeer(peer); info != nil {
			peerInfo = info
		}

		// If this peer refers to this server instance
		if peer == s.conf.AdvertiseAddress {
			peerInfo.isOwner = true
		}

		picker.Add(peerInfo)
	}

	// TODO: schedule a disconnect for old PeerClients once they are no longer in flight

	s.peerMutex.Lock()
	defer s.peerMutex.Unlock()

	// Replace our current picker
	s.conf.Picker = picker

	// Update our health status
	s.health.Status = Healthy
	if len(errs) != 0 {
		s.health.Status = UnHealthy
		s.health.Message = strings.Join(errs, "|")
	}
	s.health.PeerCount = int32(picker.Size())
	s.log.WithField("peers", conf.Peers).Debug("Peers updated")
}

func retry(attempts int, d time.Duration, callback func() error) (err error) {
	for i := 0; i < attempts; i++ {
		err = callback()
		if err == nil {
			return nil
		}
		time.Sleep(d)
	}
	return err
}