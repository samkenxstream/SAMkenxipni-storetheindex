package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	pbl "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/ipfs/go-datastore"
	leveldb "github.com/ipfs/go-ds-leveldb"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/kubo/core/bootstrap"
	"github.com/ipfs/kubo/peering"
	sth "github.com/ipld/go-storethehash/store"
	"github.com/ipni/go-indexer-core"
	"github.com/ipni/go-indexer-core/cache"
	"github.com/ipni/go-indexer-core/cache/radixcache"
	"github.com/ipni/go-indexer-core/engine"
	"github.com/ipni/go-indexer-core/store/memory"
	"github.com/ipni/go-indexer-core/store/pebble"
	"github.com/ipni/go-indexer-core/store/storethehash"
	"github.com/ipni/go-libipni/mautil"
	"github.com/ipni/storetheindex/config"
	"github.com/ipni/storetheindex/fsutil"
	"github.com/ipni/storetheindex/internal/counter"
	"github.com/ipni/storetheindex/internal/ingest"
	"github.com/ipni/storetheindex/internal/registry"
	httpadminserver "github.com/ipni/storetheindex/server/admin/http"
	httpfindserver "github.com/ipni/storetheindex/server/find/http"
	p2pfindserver "github.com/ipni/storetheindex/server/find/p2p"
	httpingestserver "github.com/ipni/storetheindex/server/ingest/http"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
)

// Recognized valuestore type names.
const (
	vstoreMemory       = "memory"
	vstorePebble       = "pebble"
	vstoreStorethehash = "sth"
)

var log = logging.Logger("indexer")

var (
	ErrDaemonStart = errors.New("daemon did not start correctly")
	ErrDaemonStop  = errors.New("daemon did not stop correctly")
)

var DaemonCmd = &cli.Command{
	Name:   "daemon",
	Usage:  "Start a network indexer daemon",
	Flags:  daemonFlags,
	Action: daemonAction,
}

var daemonFlags = []cli.Flag{
	cacheSizeFlag,
	listenAdminFlag,
	listenFindFlag,
	listenIngestFlag,
	listenP2PFlag,
	&cli.BoolFlag{
		Name:     "watch-config",
		Usage:    "Watch for changes to config file and automatically reload",
		EnvVars:  []string{"STORETHEINDEX_WATCH_CONFIG"},
		Value:    true,
		Required: false,
	},
}

func daemonAction(cctx *cli.Context) error {
	cfg, err := loadConfig("")
	if err != nil {
		if errors.Is(err, config.ErrNotInitialized) {
			fmt.Fprintln(os.Stderr, "storetheindex is not initialized")
			fmt.Fprintln(os.Stderr, "To initialize, run the command: ./storetheindex init")
			os.Exit(1)
		}
		return err
	}

	err = setLoggingConfig(cfg.Logging)
	if err != nil {
		return err
	}

	if cfg.Version != config.Version {
		log.Warn("Configuration file out-of-date. Upgrade by running: ./storetheindex init --upgrade")
	}

	var freezeDirs []string

	// Create a valuestore of the configured type.
	valueStore, minKeyLen, vsDir, err := createValueStore(cctx.Context, cfg.Indexer)
	if err != nil {
		return err
	}
	if valueStore != nil {
		log.Info("Valuestore initialized")
		// If the value store requires a minimum key length, make sure the
		// ingester is configured with at least the minimum.
		if minKeyLen > cfg.Ingest.MinimumKeyLength {
			cfg.Ingest.MinimumKeyLength = minKeyLen
		}
		freezeDirs = append(freezeDirs, vsDir)
	}

	// Create datastore
	dstore, dsDir, err := createDatastore(cfg.Datastore)
	if err != nil {
		return err
	}
	defer dstore.Close()

	freezeDirs = append(freezeDirs, dsDir)

	if cfg.Indexer.UnfreezeOnStart {
		unfrozen, err := registry.Unfreeze(cctx.Context, freezeDirs, cfg.Indexer.FreezeAtPercent, dstore)
		if err != nil {
			return fmt.Errorf("cannot unfreeze registry: %w", err)
		}
		err = ingest.Unfreeze(unfrozen, dstore)
		if err != nil {
			return fmt.Errorf("cannot unfreeze ingester: %w", err)
		}
		log.Info("Indexer reverted to unfrozen state")
	}

	// Create result cache
	var resultCache cache.Interface
	cacheSize := int(cctx.Int64("cachesize"))
	if cacheSize == 0 {
		cacheSize = cfg.Indexer.CacheSize
	}
	if cacheSize > 0 {
		resultCache = radixcache.New(cacheSize)
		log.Infow("Result cache enabled", "size", cacheSize)
	} else {
		log.Info("Result cache disabled")
	}

	// Create indexer core
	indexerCore := engine.New(resultCache, valueStore,
		engine.WithDHBatchSize(cfg.Indexer.DHBatchSize),
		engine.WithDHKeyShard(cfg.Indexer.DHEnableKeySharding),
		engine.WithDHShardConcurrency(cfg.Indexer.DHShardConcurrency),
		engine.WithDHStore(cfg.Indexer.DHStoreURL),
		engine.WithDHStoreCluster(cfg.Indexer.DHStoreClusterURLs),
		engine.WithVSNoNewMH(cfg.Indexer.VSNoNewMH),
		engine.WithHttpClientTimeout(time.Duration(cfg.Indexer.DHStoreHttpClientTimeout)),
	)

	indexCounts := counter.NewIndexCounts(dstore)
	indexCounts.SetTotalAddend(cfg.Indexer.IndexCountTotalAddend)

	// Create registry
	reg, err := registry.New(cctx.Context, cfg.Discovery, dstore,
		registry.WithFreezer(freezeDirs, cfg.Indexer.FreezeAtPercent))
	if err != nil {
		return fmt.Errorf("cannot create provider registry: %s", err)
	}

	// Create find HTTP server
	var findSvr *httpfindserver.Server
	findAddr := cfg.Addresses.Finder
	if cctx.String("listen-finder") != "" {
		findAddr = cctx.String("listen-finder")
	}
	if findAddr != "" && findAddr != "none" {
		findNetAddr, err := mautil.MultiaddrStringToNetAddr(findAddr)
		if err != nil {
			return fmt.Errorf("bad find address %s: %s", findAddr, err)
		}
		findSvr, err = httpfindserver.New(findNetAddr.String(), indexerCore, reg,
			httpfindserver.WithReadTimeout(time.Duration(cfg.Finder.ApiReadTimeout)),
			httpfindserver.WithWriteTimeout(time.Duration(cfg.Finder.ApiWriteTimeout)),
			httpfindserver.WithMaxConnections(cfg.Finder.MaxConnections),
			httpfindserver.WithHomepage(cfg.Finder.Webpage),
			httpfindserver.WithIndexCounts(indexCounts),
			httpfindserver.WithVersion(cctx.App.Version),
		)
		if err != nil {
			return err
		}
	}

	var (
		cancelP2pServers context.CancelFunc
		ingester         *ingest.Ingester
		p2pHost          host.Host
		peeringService   *peering.PeeringService
	)

	peerID, privKey, err := cfg.Identity.Decode()
	if err != nil {
		return err
	}

	// Create libp2p host and servers
	ctx, cancel := context.WithCancel(cctx.Context)
	defer cancel()

	p2pAddr := cfg.Addresses.P2PAddr
	if cctx.String("listen-p2p") != "" {
		p2pAddr = cctx.String("listen-p2p")
	}
	if p2pAddr != "" && p2pAddr != "none" {
		cancelP2pServers = cancel

		p2pmaddr, err := multiaddr.NewMultiaddr(p2pAddr)
		if err != nil {
			return fmt.Errorf("bad p2p address %s: %s", p2pAddr, err)
		}
		p2pOpts := []libp2p.Option{
			// Use the keypair generated during init
			libp2p.Identity(privKey),
			// Listen at specific address
			libp2p.ListenAddrs(p2pmaddr),
		}
		if cfg.Addresses.NoResourceManager {
			log.Info("libp2p resource manager disabled")
			p2pOpts = append(p2pOpts, libp2p.ResourceManager(&network.NullResourceManager{}))
		}

		p2pHost, err = libp2p.New(p2pOpts...)
		if err != nil {
			return err
		}

		if findSvr != nil {
			p2pfindserver.New(ctx, p2pHost, indexerCore, reg, indexCounts)
		}

		// Do not resend direct announce messages if using an assigner service.
		if cfg.Discovery.UseAssigner {
			cfg.Ingest.ResendDirectAnnounce = false
		}

		// Initialize ingester.
		ingester, err = ingest.NewIngester(cfg.Ingest, p2pHost, indexerCore, reg, dstore,
			ingest.WithIndexCounts(indexCounts))
		if err != nil {
			return err
		}

		// If there are bootstrap peers and bootstrapping is enabled, then try to
		// connect to the minimum set of peers.  This connects the indexer to other
		// nodes in the gossip mesh, allowing it to receive advertisements from
		// providers.
		if len(cfg.Bootstrap.Peers) != 0 && cfg.Bootstrap.MinimumPeers != 0 {
			addrs, err := cfg.Bootstrap.PeerAddrs()
			if err != nil {
				return fmt.Errorf("bad bootstrap peer: %s", err)
			}

			bootCfg := bootstrap.BootstrapConfigWithPeers(addrs)
			bootCfg.MinPeerThreshold = cfg.Bootstrap.MinimumPeers

			bootstrapper, err := bootstrap.Bootstrap(peerID, p2pHost, nil, bootCfg)
			if err != nil {
				return fmt.Errorf("bootstrap failed: %s", err)
			}
			defer bootstrapper.Close()
		}

		peeringService, err = reloadPeering(cfg.Peering, nil, p2pHost)
		if err != nil {
			return fmt.Errorf("error loading peering service: %s", err)
		}

		log.Infow("libp2p servers initialized", "host_id", p2pHost.ID(), "multiaddr", p2pmaddr)
	}

	// Create ingest HTTP server
	var ingestSvr *httpingestserver.Server
	ingestAddr := cfg.Addresses.Ingest
	if cctx.String("listen-ingest") != "" {
		ingestAddr = cctx.String("listen-ingest")
	}
	if ingestAddr != "" && ingestAddr != "none" {
		ingestNetAddr, err := mautil.MultiaddrStringToNetAddr(ingestAddr)
		if err != nil {
			return fmt.Errorf("bad ingest address %s: %s", ingestAddr, err)
		}
		ingestSvr, err = httpingestserver.New(ingestNetAddr.String(), indexerCore, ingester, reg,
			httpingestserver.WithVersion(cctx.App.Version))
		if err != nil {
			return err
		}
	}

	reloadErrsChan := make(chan chan error, 1)

	// Create admin HTTP server
	var adminSvr *httpadminserver.Server
	adminAddr := cfg.Addresses.Admin
	if cctx.String("listen-admin") != "" {
		adminAddr = cctx.String("listen-admin")
	}
	if adminAddr != "" && adminAddr != "none" {
		adminNetAddr, err := mautil.MultiaddrStringToNetAddr(adminAddr)
		if err != nil {
			return fmt.Errorf("bad admin address %s: %s", adminAddr, err)
		}
		adminSvr, err = httpadminserver.New(adminNetAddr.String(), peerID, indexerCore, ingester, reg, reloadErrsChan)
		if err != nil {
			return err
		}
	}

	svrErrChan := make(chan error, 3)

	log.Info("Starting http servers")
	if findSvr != nil {
		go func() {
			svrErrChan <- findSvr.Start()
		}()
		fmt.Println("Find server:\t", cfg.Addresses.Finder)
	} else {
		fmt.Println("Find server:\t disabled")
	}
	if ingestSvr != nil {
		go func() {
			svrErrChan <- ingestSvr.Start()
		}()
		fmt.Println("Ingest server:\t", cfg.Addresses.Ingest)
	} else {
		fmt.Println("Ingest server:\t disabled")
	}
	if adminSvr != nil {
		go func() {
			svrErrChan <- adminSvr.Start()
		}()
		fmt.Println("Admin server:\t", cfg.Addresses.Admin)
	} else {
		fmt.Println("Admin server:\t disabled")
	}

	reloadSig := make(chan os.Signal, 1)
	signal.Notify(reloadSig, syscall.SIGHUP)

	// Output message to user (not to log).
	if cfg.Discovery.UseAssigner {
		fmt.Println("Indexer configured to use assigner service")
	}
	if reg.Frozen() {
		fmt.Println("Indexer is frozen")
	}
	fmt.Println("Indexer is ready")

	var cfgPath string
	if cctx.Bool("watch-config") {
		cfgPath, err = config.Filename("")
		if err != nil {
			log.Errorw("Cannot get config file name", "err", err)
		}
	}

	var finalErr, statErr error
	var modTime time.Time
	var ticker *time.Ticker
	var timeChan <-chan time.Time

	if cfgPath != "" {
		modTime, _, statErr = fsutil.FileChanged(cfgPath, modTime)
		if statErr != nil {
			log.Error(err)
		}
		ticker = time.NewTicker(time.Duration(cfg.Indexer.ConfigCheckInterval))
		timeChan = ticker.C
	}

	shutdownTimeout := cfg.Indexer.ShutdownTimeout

	for endDaemon := false; !endDaemon; {
		select {
		case <-cctx.Done():
			// Command was canceled (ctrl-c)
			endDaemon = true
		case err = <-svrErrChan:
			log.Errorw("Failed to start server", "err", err)
			finalErr = ErrDaemonStart
			endDaemon = true
		case <-reloadSig:
			reloadErrsChan <- nil
		case errChan := <-reloadErrsChan:
			// A reload has been triggered by putting either an error channel
			// or nil on reloadErrsChan. If the reload signaler wants to know
			// if an error occurred, the the error channel is not nil.
			prevCfgChk := cfg.Indexer.ConfigCheckInterval
			if prevCfgChk != cfg.Indexer.ConfigCheckInterval {
				ticker.Reset(time.Duration(cfg.Indexer.ConfigCheckInterval))
			}

			cfg, err = reloadConfig(cfgPath, ingester, reg, valueStore)
			if err != nil {
				log.Errorw("Error reloading conifg", "err", err)
				if errChan != nil {
					errChan <- errors.New("could not reload configuration")
				}
				continue
			}
			shutdownTimeout = cfg.Indexer.ShutdownTimeout

			if p2pHost != nil {
				peeringService, err = reloadPeering(cfg.Peering, peeringService, p2pHost)
				if err != nil {
					log.Errorw("Error reloading peering service", "err", err)
					if errChan != nil {
						errChan <- errors.New("could not reload peering service")
						continue
					}
				}
			}

			if indexCounts != nil {
				indexCounts.SetTotalAddend(cfg.Indexer.IndexCountTotalAddend)
			}

			if errChan != nil {
				errChan <- nil
			}
		case <-timeChan:
			var changed bool
			modTime, changed, err = fsutil.FileChanged(cfgPath, modTime)
			if err != nil {
				if statErr == nil {
					log.Errorw("Cannot stat config file", "err", err, "path", cfgPath)
					statErr = err
				}
				continue
			}
			statErr = nil
			if changed {
				reloadErrsChan <- nil
			}
		}
	}
	if ticker != nil {
		ticker.Stop()
	}

	log.Infow("Shutting down daemon")

	// If a shutdown timeout is configured, then wait that amount of time for a
	// gradeful shutdown to before exiting with error.
	if shutdownTimeout > 0 {
		shCtx, shCancel := context.WithTimeout(context.Background(), time.Duration(shutdownTimeout))
		defer shCancel()

		go func() {
			// Wait for context to be canceled. Exit with error if timeout.
			<-shCtx.Done()
			if shCtx.Err() == context.DeadlineExceeded {
				fmt.Println("Timed out on shutdown, terminating...")
				os.Exit(-1)
			}
		}()
	}

	if peeringService != nil {
		err = peeringService.Stop()
		if err != nil {
			log.Errorw("Error stopping peering service", "err", err)
		}
	}

	if cancelP2pServers != nil {
		cancelP2pServers()
	}

	if ingestSvr != nil {
		if err = ingestSvr.Close(); err != nil {
			log.Errorw("Error shutting down ingest server", "err", err)
			finalErr = ErrDaemonStop
		}
	}
	if findSvr != nil {
		if err = findSvr.Close(); err != nil {
			log.Errorw("Error shutting down find server", "err", err)
			finalErr = ErrDaemonStop
		}
	}
	if adminSvr != nil {
		if err = adminSvr.Close(); err != nil {
			log.Errorw("Error shutting down admin server", "err", err)
			finalErr = ErrDaemonStop
		}
	}

	// If ingester set, close ingester
	if ingester != nil {
		if err = ingester.Close(); err != nil {
			log.Errorw("Error closing ingester", "err", err)
			finalErr = ErrDaemonStop
		}
	}

	if valueStore != nil {
		if err = valueStore.Close(); err != nil {
			log.Errorw("Error closing value store", "err", err)
			finalErr = ErrDaemonStop
		}
	}

	reg.Close()
	dstore.Close()

	log.Info("Indexer stopped")
	return finalErr
}

func createValueStore(ctx context.Context, cfgIndexer config.Indexer) (indexer.Interface, int, string, error) {
	const sthMinKeyLen = 4

	if cfgIndexer.ValueStoreType == "" || cfgIndexer.ValueStoreType == "none" {
		return nil, 0, "", nil
	}

	dir, err := config.Path("", cfgIndexer.ValueStoreDir)
	if err != nil {
		return nil, 0, "", err
	}
	log.Infow("Valuestore initializing/opening", "type", cfgIndexer.ValueStoreType, "path", dir)

	if err = fsutil.DirWritable(dir); err != nil {
		return nil, 0, "", err
	}

	var vs indexer.Interface
	var minKeyLen int

	switch cfgIndexer.ValueStoreType {
	case vstoreStorethehash:
		if cfgIndexer.GCInterval == -1 {
			cfgIndexer.GCInterval = 0
			cfgIndexer.GCTimeLimit = 0
		}
		vs, err = storethehash.New(
			ctx,
			dir,
			cfgIndexer.CorePutConcurrency,
			sth.GCInterval(time.Duration(cfgIndexer.GCInterval)),
			sth.GCTimeLimit(time.Duration(cfgIndexer.GCTimeLimit)),
			sth.BurstRate(cfgIndexer.STHBurstRate),
			sth.SyncInterval(time.Duration(cfgIndexer.STHSyncInterval)),
			sth.IndexBitSize(cfgIndexer.STHBits),
			sth.FileCacheSize(cfgIndexer.STHFileCacheSize),
		)
		minKeyLen = sthMinKeyLen
	case vstoreMemory:
		vs, err = memory.New(), nil
	case vstorePebble:

		// TODO: parameterize values and study what settings are right for sti

		// Default options copied from cockroachdb with the addition of 1GiB cache.
		// See:
		// - https://github.com/cockroachdb/cockroach/blob/v22.1.6/pkg/storage/pebble.go#L479
		pebbleOpts := &pbl.Options{
			BytesPerSync:                10 << 20, // 10 MiB
			WALBytesPerSync:             10 << 20, // 10 MiB
			MaxConcurrentCompactions:    10,
			MemTableSize:                64 << 20, // 64 MiB
			MemTableStopWritesThreshold: 4,
			LBaseMaxBytes:               64 << 20, // 64 MiB
			L0CompactionThreshold:       2,
			L0StopWritesThreshold:       1000,
			DisableWAL:                  cfgIndexer.PebbleDisableWAL,
			WALMinSyncInterval:          func() time.Duration { return 30 * time.Second },
		}

		pebbleOpts.Experimental.ReadCompactionRate = 10 << 20 // 20 MiB
		pebbleOpts.Experimental.MinDeletionRate = 128 << 20   // 128 MiB

		const numLevels = 7
		pebbleOpts.Levels = make([]pbl.LevelOptions, numLevels)
		for i := 0; i < numLevels; i++ {
			l := &pebbleOpts.Levels[i]
			l.BlockSize = 32 << 10       // 32 KiB
			l.IndexBlockSize = 256 << 10 // 256 KiB
			l.FilterPolicy = bloom.FilterPolicy(10)
			l.FilterType = pbl.TableFilter
			if i > 0 {
				l.TargetFileSize = pebbleOpts.Levels[i-1].TargetFileSize * 2
			}
			l.EnsureDefaults()
		}
		pebbleOpts.Levels[numLevels-1].FilterPolicy = nil
		pebbleOpts.Cache = pbl.NewCache(int64(cfgIndexer.PebbleBlockCacheSize))

		vs, err = pebble.New(dir, pebbleOpts)
	default:
		err = fmt.Errorf("unrecognized store type: %s", cfgIndexer.ValueStoreType)
	}
	if err != nil {
		return nil, 0, "", fmt.Errorf("cannot create %s value store: %w", cfgIndexer.ValueStoreType, err)
	}
	return vs, minKeyLen, dir, nil
}

func setLoggingConfig(cfgLogging config.Logging) error {
	// Set overall log level.
	err := logging.SetLogLevel("*", cfgLogging.Level)
	if err != nil {
		return err
	}

	// Set level for individual loggers.
	for loggerName, level := range cfgLogging.Loggers {
		err = logging.SetLogLevel(loggerName, level)
		if err != nil {
			return err
		}
	}
	return nil
}

func loadConfig(filePath string) (*config.Config, error) {
	cfg, err := config.Load(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot load config file: %w", err)
	}
	if cfg.Version != config.Version {
		log.Warn("Configuration file out-of-date. Upgrade by running: ./storetheindex init --upgrade")
	}

	if cfg.Datastore.Type != "levelds" {
		return nil, fmt.Errorf("only levelds datastore type supported, %q not supported", cfg.Datastore.Type)
	}

	return cfg, nil
}

func reloadConfig(cfgPath string, ingester *ingest.Ingester, reg *registry.Registry, valueStore indexer.Interface) (*config.Config, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	err = reg.SetPolicy(cfg.Discovery.Policy)
	if err != nil {
		return nil, fmt.Errorf("failed to set policy config: %w", err)
	}

	if ingester != nil {
		err = ingester.SetRateLimit(cfg.Ingest.RateLimit)
		if err != nil {
			return nil, fmt.Errorf("failed to set rate limit config: %w", err)
		}
		ingester.RunWorkers(cfg.Ingest.IngestWorkerCount)
	}

	err = setLoggingConfig(cfg.Logging)
	if err != nil {
		return nil, fmt.Errorf("failed to configure logging: %w", err)
	}

	if valueStore != nil {
		sthStore, ok := valueStore.(*storethehash.SthStorage)
		if ok {
			sthStore.SetPutConcurrency(cfg.Indexer.CorePutConcurrency)
			sthStore.SetFileCacheSize(cfg.Indexer.STHFileCacheSize)
		}
	}

	log.Info("Reloaded reloadable values from configuration")
	return cfg, nil
}

func reloadPeering(cfg config.Peering, peeringService *peering.PeeringService, p2pHost host.Host) (*peering.PeeringService, error) {
	// If no peers are configured, then stop peering service if it is running.
	if len(cfg.Peers) == 0 {
		if peeringService != nil {
			err := peeringService.Stop()
			if err != nil {
				return nil, fmt.Errorf("error stopping peering service: %w", err)
			}
		}
		return nil, nil
	}

	curPeers, err := cfg.PeerAddrs()
	if err != nil {
		return nil, fmt.Errorf("bad peering peer: %s", err)
	}

	// If peering service is not running, add peers and start service.
	if peeringService == nil {
		peeringService = peering.NewPeeringService(p2pHost)
		for i := range curPeers {
			peeringService.AddPeer(curPeers[i])
		}
		err = peeringService.Start()
		if err != nil {
			return nil, fmt.Errorf("failed to start peering service: %w", err)
		}
		return peeringService, nil
	}

	// Peering service is running, so remove peers that are no longer listed.
	prevPeers := peeringService.ListPeers()

	for _, prev := range prevPeers {
		found := false
		for _, cur := range curPeers {
			if cur.ID == prev.ID {
				found = true
				break
			}
		}
		if !found {
			peeringService.RemovePeer(prev.ID)
		}
	}

	for i := range curPeers {
		peeringService.AddPeer(curPeers[i])
	}

	return peeringService, nil
}

func createDatastore(cfg config.Datastore) (datastore.Batching, string, error) {
	if cfg.Type != "levelds" {
		return nil, "", fmt.Errorf("only levelds datastore type supported, %q not supported", cfg.Type)
	}
	dataStorePath, err := config.Path("", cfg.Dir)
	if err != nil {
		return nil, "", err
	}
	if err = fsutil.DirWritable(dataStorePath); err != nil {
		return nil, "", err
	}
	ds, err := leveldb.NewDatastore(dataStorePath, nil)
	if err != nil {
		return nil, "", err
	}
	return ds, dataStorePath, nil
}
