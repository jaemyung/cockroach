// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package server

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/ts"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
)

const (
	// TestUser is a fixed user used in unittests.
	// It has valid embedded client certs.
	TestUser = "testuser"
	// initialSplitsTimeout is the amount of time to wait for initial splits to
	// occur on a freshly started server.
	// Note: this needs to be fairly high or tests become flaky.
	initialSplitsTimeout = 10 * time.Second
)

// makeTestConfig returns a config for testing. It overrides the
// Certs with the test certs directory.
// We need to override the certs loader.
func makeTestConfig() Config {
	cfg := MakeConfig()

	// MaxOffset is the maximum offset for clocks in the cluster.
	// This is mostly irrelevant except when testing reads within
	// uncertainty intervals.
	cfg.MaxOffset = 50 * time.Millisecond

	// Test servers start in secure mode by default.
	cfg.Insecure = false

	// Load test certs. In addition, the tests requiring certs
	// need to call security.SetReadFileFn(securitytest.Asset)
	// in their init to mock out the file system calls for calls to AssetFS,
	// which has the test certs compiled in. Typically this is done
	// once per package, in main_test.go.
	cfg.SSLCA = filepath.Join(security.EmbeddedCertsDir, security.EmbeddedCACert)
	cfg.SSLCert = filepath.Join(security.EmbeddedCertsDir, security.EmbeddedNodeCert)
	cfg.SSLCertKey = filepath.Join(security.EmbeddedCertsDir, security.EmbeddedNodeKey)

	// Addr defaults to localhost with port set at time of call to
	// Start() to an available port. May be overridden later (as in
	// makeTestConfigFromParams). Call TestServer.ServingAddr() for the
	// full address (including bound port).
	cfg.Addr = util.TestAddr.String()
	cfg.AdvertiseAddr = util.TestAddr.String()
	cfg.HTTPAddr = util.TestAddr.String()
	// Set standard user for intra-cluster traffic.
	cfg.User = security.NodeUser
	cfg.MetricsSampleInterval = metric.TestSampleInterval

	return cfg
}

// makeTestConfigtFromParams creates a Config from a TestServerParams.
func makeTestConfigFromParams(params base.TestServerArgs) Config {
	cfg := makeTestConfig()
	cfg.TestingKnobs = params.Knobs
	if params.JoinAddr != "" {
		cfg.JoinList = []string{params.JoinAddr}
	}
	cfg.Insecure = params.Insecure
	cfg.SocketFile = params.SocketFile
	if params.MetricsSampleInterval != time.Duration(0) {
		cfg.MetricsSampleInterval = params.MetricsSampleInterval
	}
	if params.MaxOffset != time.Duration(0) {
		cfg.MaxOffset = params.MaxOffset
	}
	if params.ScanInterval != time.Duration(0) {
		cfg.ScanInterval = params.ScanInterval
	}
	if params.ScanMaxIdleTime != time.Duration(0) {
		cfg.ScanMaxIdleTime = params.ScanMaxIdleTime
	}
	if params.SSLCA != "" {
		cfg.SSLCA = params.SSLCA
	}
	if params.SSLCert != "" {
		cfg.SSLCert = params.SSLCert
	}
	if params.SSLCertKey != "" {
		cfg.SSLCertKey = params.SSLCertKey
	}
	if params.DisableEventLog {
		cfg.EventLogEnabled = false
	}
	cfg.JoinList = []string{params.JoinAddr}
	if cfg.Insecure {
		// Whenever we can (i.e. in insecure mode), use IsolatedTestAddr
		// to prevent issues that can occur when running a test under
		// stress.
		cfg.Addr = util.IsolatedTestAddr.String()
		cfg.AdvertiseAddr = util.IsolatedTestAddr.String()
		cfg.HTTPAddr = util.IsolatedTestAddr.String()
	} else {
		cfg.Addr = util.TestAddr.String()
		cfg.AdvertiseAddr = util.TestAddr.String()
		cfg.HTTPAddr = util.TestAddr.String()
	}
	return cfg
}

// A TestServer encapsulates an in-memory instantiation of a cockroach node with
// a single store. It provides tests with access to Server internals.
// Where possible, it should be used through the
// testingshim.TestServerInterface.
//
// Example usage of a TestServer:
//
//   s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
//   defer s.Stopper().Stop()
//   // If really needed, in tests that can depend on server, downcast to
//   // server.TestServer:
//   ts := s.(*server.TestServer)
//
type TestServer struct {
	Cfg *Config
	// server is the embedded Cockroach server struct.
	*Server
}

// Stopper returns the embedded server's Stopper.
func (ts *TestServer) Stopper() *stop.Stopper {
	return ts.stopper
}

// Gossip returns the gossip instance used by the TestServer.
func (ts *TestServer) Gossip() *gossip.Gossip {
	if ts != nil {
		return ts.gossip
	}
	return nil
}

// Clock returns the clock used by the TestServer.
func (ts *TestServer) Clock() *hlc.Clock {
	if ts != nil {
		return ts.clock
	}
	return nil
}

// RPCContext returns the rpc context used by the TestServer.
func (ts *TestServer) RPCContext() *rpc.Context {
	if ts != nil {
		return ts.rpcContext
	}
	return nil
}

// TsDB returns the ts.DB instance used by the TestServer.
func (ts *TestServer) TsDB() *ts.DB {
	if ts != nil {
		return ts.tsDB
	}
	return nil
}

// DB returns the client.DB instance used by the TestServer.
func (ts *TestServer) DB() *client.DB {
	if ts != nil {
		return ts.db
	}
	return nil
}

// Start starts the TestServer by bootstrapping an in-memory store
// (defaults to maximum of 100M). The server is started, launching the
// node RPC server and all HTTP endpoints. Use the value of
// TestServer.ServingAddr() after Start() for client connections.
// Use TestServer.Stopper().Stop() to shutdown the server after the test
// completes.
func (ts *TestServer) Start(params base.TestServerArgs) error {
	if ts.Cfg == nil {
		panic("Cfg not set")
	}

	if params.Stopper == nil {
		params.Stopper = stop.NewStopper()
	}

	if !params.PartOfCluster {
		// Change the replication requirements so we don't get log spam about ranges
		// not being replicated enough.
		cfg := config.DefaultZoneConfig()
		cfg.NumReplicas = 1
		fn := config.TestingSetDefaultZoneConfig(cfg)
		params.Stopper.AddCloser(stop.CloserFn(fn))
	}

	// Needs to be called before NewServer to ensure resolvers are initialized.
	if err := ts.Cfg.InitNode(); err != nil {
		return err
	}

	// Ensure we have the correct number of engines. Add in-memory ones where
	// needed. There must be at least one store/engine.
	if len(params.StoreSpecs) == 0 {
		params.StoreSpecs = []base.StoreSpec{base.DefaultTestStoreSpec}
	}
	for _, storeSpec := range params.StoreSpecs {
		if storeSpec.InMemory {
			if storeSpec.SizePercent > 0 {
				panic(fmt.Sprintf("test server does not yet support in memory stores based on percentage of total memory: %s", storeSpec))
			}
			ts.Cfg.Engines = append(ts.Cfg.Engines, engine.NewInMem(
				roachpb.Attributes{},
				storeSpec.SizeInBytes,
				params.Stopper,
			))
		} else {
			// TODO(bram): This will require some cleanup of on disk files.
			panic(fmt.Sprintf("test server does not yet support on disk stores: %s", storeSpec))
		}
	}

	var err error
	ts.Server, err = NewServer(*ts.Cfg, params.Stopper)
	if err != nil {
		return err
	}
	// Our context must be shared with our server.
	ts.Cfg = &ts.Server.cfg

	if err := ts.Server.Start(context.Background()); err != nil {
		return err
	}

	// If enabled, wait for initial splits to complete before returning control.
	// If initial splits do not complete, the server is stopped before
	// returning.
	if stk, ok := ts.cfg.TestingKnobs.Store.(*storage.StoreTestingKnobs); ok &&
		stk.DisableSplitQueue {
		return nil
	}
	if err := ts.WaitForInitialSplits(); err != nil {
		ts.Stop()
		return err
	}

	return nil
}

// ExpectedInitialRangeCount returns the expected number of ranges that should
// be on the server after initial (asynchronous) splits have been completed,
// assuming no additional information is added outside of the normal bootstrap
// process.
func ExpectedInitialRangeCount() int {
	bootstrap := GetBootstrapSchema()
	return bootstrap.SystemDescriptorCount() - bootstrap.SystemConfigDescriptorCount() + 1
}

// WaitForInitialSplits waits for the server to complete its expected initial
// splits at startup. If the expected range count is not reached within a
// configured timeout, an error is returned.
func (ts *TestServer) WaitForInitialSplits() error {
	return WaitForInitialSplits(ts.DB())
}

// WaitForInitialSplits waits for the expected number of initial ranges to be
// populated in the meta2 table. If the expected range count is not reached
// within a configured timeout, an error is returned.
func WaitForInitialSplits(db *client.DB) error {
	expectedRanges := ExpectedInitialRangeCount()
	return util.RetryForDuration(initialSplitsTimeout, func() error {
		// Scan all keys in the Meta2Prefix; we only need a count.
		rows, err := db.Scan(context.TODO(), keys.Meta2Prefix, keys.MetaMax, 0)
		if err != nil {
			return err
		}
		if a, e := len(rows), expectedRanges; a != e {
			return errors.Errorf("had %d ranges at startup, expected %d", a, e)
		}
		return nil
	})
}

// Stores returns the collection of stores from this TestServer's node.
func (ts *TestServer) Stores() *storage.Stores {
	return ts.node.stores
}

// ServingAddr returns the server's address. Should be used by clients.
func (ts *TestServer) ServingAddr() string {
	return ts.cfg.AdvertiseAddr
}

// ServingHost returns the host portion of the rpc server's address.
func (ts *TestServer) ServingHost() (string, error) {
	h, _, err := net.SplitHostPort(ts.ServingAddr())
	return h, err
}

// ServingPort returns the port portion of the rpc server's address.
func (ts *TestServer) ServingPort() (string, error) {
	_, p, err := net.SplitHostPort(ts.ServingAddr())
	return p, err
}

// WriteSummaries implements TestServerInterface.
func (ts *TestServer) WriteSummaries() error {
	return ts.node.writeSummaries(context.TODO())
}

// AdminURL implements TestServerInterface.
func (ts *TestServer) AdminURL() string {
	return ts.Cfg.AdminURL()
}

// GetHTTPClient implements TestServerInterface.
func (ts *TestServer) GetHTTPClient() (http.Client, error) {
	return ts.Cfg.GetHTTPClient()
}

// MustGetSQLCounter implements TestServerInterface.
func (ts *TestServer) MustGetSQLCounter(name string) int64 {
	var c int64
	var found bool

	ts.registry.Each(func(n string, v interface{}) {
		if name == n {
			c = v.(*metric.Counter).Count()
			found = true
		}
	})
	if !found {
		panic(fmt.Sprintf("couldn't find metric %s", name))
	}
	return c
}

// MustGetSQLNetworkCounter implements TestServerInterface.
func (ts *TestServer) MustGetSQLNetworkCounter(name string) int64 {
	var c int64
	var found bool

	reg := metric.NewRegistry()
	reg.AddMetricStruct(ts.pgServer.Metrics())
	reg.Each(func(n string, v interface{}) {
		if name == n {
			c = v.(*metric.Counter).Count()
			found = true
		}
	})
	if !found {
		panic(fmt.Sprintf("couldn't find metric %s", name))
	}
	return c
}

// KVClient is part of TestServerInterface.
func (ts *TestServer) KVClient() interface{} { return ts.db }

// KVDB is part of TestServerInterface.
func (ts *TestServer) KVDB() interface{} { return ts.kvDB }

// LeaseManager is part of TestServerInterface.
func (ts *TestServer) LeaseManager() interface{} {
	return ts.leaseMgr
}

// GetNode exposes the Server's Node.
func (ts *TestServer) GetNode() *Node {
	return ts.node
}

// DistSender exposes the Server's DistSender.
func (ts *TestServer) DistSender() *kv.DistSender {
	return ts.distSender
}

// GetFirstStoreID is a utility function returning the StoreID of the first
// store on this node.
func (ts *TestServer) GetFirstStoreID() roachpb.StoreID {
	firstStoreID := roachpb.StoreID(-1)
	err := ts.Stores().VisitStores(func(s *storage.Store) error {
		if firstStoreID == -1 {
			firstStoreID = s.Ident.StoreID
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return firstStoreID
}

type testServerFactoryImpl struct{}

// TestServerFactory can be passed to serverutils.InitTestServerFactory
var TestServerFactory = testServerFactoryImpl{}

// New is part of TestServerFactory interface.
func (testServerFactoryImpl) New(params base.TestServerArgs) interface{} {
	cfg := makeTestConfigFromParams(params)
	return &TestServer{Cfg: &cfg}
}
