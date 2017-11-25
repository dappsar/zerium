// Copyright 2014 The zerium Authors
// This file is part of the zerium library.
//
// The zerium library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The zerium library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the zerium library. If not, see <http://www.gnu.org/licenses/>.

// Package zrm implements the Zerium protocol.
package zrm

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/abt/zerium/accounts"
	"github.com/abt/zerium/common"
	"github.com/abt/zerium/common/hexutil"
	"github.com/abt/zerium/consensus"
	"github.com/abt/zerium/consensus/clique"
	"github.com/abt/zerium/consensus/zrmash"
	"github.com/abt/zerium/core"
	"github.com/abt/zerium/core/bloombits"
	"github.com/abt/zerium/core/types"
	"github.com/abt/zerium/core/vm"
	"github.com/abt/zerium/zrm/downloader"
	"github.com/abt/zerium/zrm/filters"
	"github.com/abt/zerium/zrm/gasprice"
	"github.com/abt/zerium/zrmdb"
	"github.com/abt/zerium/event"
	"github.com/abt/zerium/internal/ethapi"
	"github.com/abt/zerium/log"
	"github.com/abt/zerium/miner"
	"github.com/abt/zerium/node"
	"github.com/abt/zerium/p2p"
	"github.com/abt/zerium/params"
	"github.com/abt/zerium/rlp"
	"github.com/abt/zerium/rpc"
)

type LesServer interface {
	Start(srvr *p2p.Server)
	Stop()
	Protocols() []p2p.Protocol
	SetBloomBitsIndexer(bbIndexer *core.ChainIndexer)
}

// Zerium implements the Zerium full node service.
type Zerium struct {
	config      *Config
	chainConfig *params.ChainConfig

	// Channel for shutting down the service
	shutdownChan  chan bool    // Channel for shutting down the abt
	stopDbUpgrade func() error // stop chain db sequential key upgrade

	// Handlers
	txPool          *core.TxPool
	blockchain      *core.BlockChain
	protocolManager *ProtocolManager
	lesServer       LesServer

	// DB interfaces
	chainDb zrmdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer  *core.ChainIndexer             // Bloom indexer operating during block imports

	ApiBackend *EthApiBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	zeriumbase common.Address

	networkId     uint64
	netRPCService *ethapi.PublicNetAPI

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and zeriumbase)
}

func (s *Zerium) AddLesServer(ls LesServer) {
	s.lesServer = ls
	ls.SetBloomBitsIndexer(s.bloomIndexer)
}

// New creates a new Zerium object (including the
// initialisation of the common Zerium object)
func New(ctx *node.ServiceContext, config *Config) (*Zerium, error) {
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run zrm.Zerium in light sync mode, use les.LightZerium")
	}
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	chainDb, err := CreateDB(ctx, config, "chaindata")
	if err != nil {
		return nil, err
	}
	stopDbUpgrade := upgradeDeduplicateData(chainDb)
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	zrm := &Zerium{
		config:         config,
		chainDb:        chainDb,
		chainConfig:    chainConfig,
		eventMux:       ctx.EventMux,
		accountManager: ctx.AccountManager,
		engine:         CreateConsensusEngine(ctx, config, chainConfig, chainDb),
		shutdownChan:   make(chan bool),
		stopDbUpgrade:  stopDbUpgrade,
		networkId:      config.NetworkId,
		gasPrice:       config.GasPrice,
		zeriumbase:      config.Zeriumbase,
		bloomRequests:  make(chan chan *bloombits.Retrieval),
		bloomIndexer:   NewBloomIndexer(chainDb, params.BloomBitsBlocks),
	}

	log.Info("Initialising Zerium protocol", "versions", ProtocolVersions, "network", config.NetworkId)

	if !config.SkipBcVersionCheck {
		bcVersion := core.GetBlockChainVersion(chainDb)
		if bcVersion != core.BlockChainVersion && bcVersion != 0 {
			return nil, fmt.Errorf("Blockchain DB version mismatch (%d / %d). Run gzrm upgradedb.\n", bcVersion, core.BlockChainVersion)
		}
		core.WriteBlockChainVersion(chainDb, core.BlockChainVersion)
	}

	vmConfig := vm.Config{EnablePreimageRecording: config.EnablePreimageRecording}
	zrm.blockchain, err = core.NewBlockChain(chainDb, zrm.chainConfig, zrm.engine, vmConfig)
	if err != nil {
		return nil, err
	}
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		zrm.blockchain.SetHead(compat.RewindTo)
		core.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	zrm.bloomIndexer.Start(zrm.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = ctx.ResolvePath(config.TxPool.Journal)
	}
	zrm.txPool = core.NewTxPool(config.TxPool, zrm.chainConfig, zrm.blockchain)

	if zrm.protocolManager, err = NewProtocolManager(zrm.chainConfig, config.SyncMode, config.NetworkId, zrm.eventMux, zrm.txPool, zrm.engine, zrm.blockchain, chainDb); err != nil {
		return nil, err
	}
	zrm.miner = miner.New(zrm, zrm.chainConfig, zrm.EventMux(), zrm.engine)
	zrm.miner.SetExtra(makeExtraData(config.ExtraData))

	zrm.ApiBackend = &EthApiBackend{zrm, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.GasPrice
	}
	zrm.ApiBackend.gpo = gasprice.NewOracle(zrm.ApiBackend, gpoParams)

	return zrm, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"gzrm",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}
	return extra
}

// CreateDB creates the chain database.
func CreateDB(ctx *node.ServiceContext, config *Config, name string) (zrmdb.Database, error) {
	db, err := ctx.OpenDatabase(name, config.DatabaseCache, config.DatabaseHandles)
	if err != nil {
		return nil, err
	}
	if db, ok := db.(*zrmdb.LDBDatabase); ok {
		db.Meter("zrm/db/chaindata/")
	}
	return db, nil
}

// CreateConsensusEngine creates the required type of consensus engine instance for an Zerium service
func CreateConsensusEngine(ctx *node.ServiceContext, config *Config, chainConfig *params.ChainConfig, db zrmdb.Database) consensus.Engine {
	// If proof-of-authority is requested, set it up
	if chainConfig.Clique != nil {
		return clique.New(chainConfig.Clique, db)
	}
	// Otherwise assume proof-of-work
	switch {
	case config.PowFake:
		log.Warn("Ethash used in fake mode")
		return zrmash.NewFaker()
	case config.PowTest:
		log.Warn("Ethash used in test mode")
		return zrmash.NewTester()
	case config.PowShared:
		log.Warn("Ethash used in shared mode")
		return zrmash.NewShared()
	default:
		engine := zrmash.New(ctx.ResolvePath(config.EthashCacheDir), config.EthashCachesInMem, config.EthashCachesOnDisk,
			config.EthashDatasetDir, config.EthashDatasetsInMem, config.EthashDatasetsOnDisk)
		engine.SetThreads(-1) // Disable CPU mining
		return engine
	}
}

// APIs returns the collection of RPC services the abt package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Zerium) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.ApiBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "zrm",
			Version:   "1.0",
			Service:   NewPublicZeriumAPI(s),
			Public:    true,
		}, {
			Namespace: "zrm",
			Version:   "1.0",
			Service:   NewPublicMinerAPI(s),
			Public:    true,
		}, {
			Namespace: "zrm",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.protocolManager.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "miner",
			Version:   "1.0",
			Service:   NewPrivateMinerAPI(s),
			Public:    false,
		}, {
			Namespace: "zrm",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.ApiBackend, false),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s.chainConfig, s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *Zerium) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Zerium) Zeriumbase() (eb common.Address, err error) {
	s.lock.RLock()
	zeriumbase := s.zeriumbase
	s.lock.RUnlock()

	if zeriumbase != (common.Address{}) {
		return zeriumbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			return accounts[0].Address, nil
		}
	}
	return common.Address{}, fmt.Errorf("zeriumbase address must be explicitly specified")
}

// set in js console via admin interface or wrapper from cli flags
func (self *Zerium) SetZeriumbase(zeriumbase common.Address) {
	self.lock.Lock()
	self.zeriumbase = zeriumbase
	self.lock.Unlock()

	self.miner.SetZeriumbase(zeriumbase)
}

func (s *Zerium) StartMining(local bool) error {
	eb, err := s.Zeriumbase()
	if err != nil {
		log.Error("Cannot start mining without zeriumbase", "err", err)
		return fmt.Errorf("zeriumbase missing: %v", err)
	}
	if clique, ok := s.engine.(*clique.Clique); ok {
		wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
		if wallet == nil || err != nil {
			log.Error("Zeriumbase account unavailable locally", "err", err)
			return fmt.Errorf("signer missing: %v", err)
		}
		clique.Authorize(eb, wallet.SignHash)
	}
	if local {
		// If local (CPU) mining is started, we can disable the transaction rejection
		// mechanism introduced to speed sync times. CPU mining on mainnet is ludicrous
		// so noone will ever hit this path, whereas marking sync done on CPU mining
		// will ensure that private networks work in single miner mode too.
		atomic.StoreUint32(&s.protocolManager.acceptTxs, 1)
	}
	go s.miner.Start(eb)
	return nil
}

func (s *Zerium) StopMining()         { s.miner.Stop() }
func (s *Zerium) IsMining() bool      { return s.miner.Mining() }
func (s *Zerium) Miner() *miner.Miner { return s.miner }

func (s *Zerium) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Zerium) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Zerium) TxPool() *core.TxPool               { return s.txPool }
func (s *Zerium) EventMux() *event.TypeMux           { return s.eventMux }
func (s *Zerium) Engine() consensus.Engine           { return s.engine }
func (s *Zerium) ChainDb() zrmdb.Database            { return s.chainDb }
func (s *Zerium) IsListening() bool                  { return true } // Always listening
func (s *Zerium) EthVersion() int                    { return int(s.protocolManager.SubProtocols[0].Version) }
func (s *Zerium) NetVersion() uint64                 { return s.networkId }
func (s *Zerium) Downloader() *downloader.Downloader { return s.protocolManager.downloader }

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *Zerium) Protocols() []p2p.Protocol {
	if s.lesServer == nil {
		return s.protocolManager.SubProtocols
	}
	return append(s.protocolManager.SubProtocols, s.lesServer.Protocols()...)
}

// Start implements node.Service, starting all internal goroutines needed by the
// Zerium protocol implementation.
func (s *Zerium) Start(srvr *p2p.Server) error {
	// Start the bloom bits servicing goroutines
	s.startBloomHandlers()

	// Start the RPC service
	s.netRPCService = ethapi.NewPublicNetAPI(srvr, s.NetVersion())

	// Figure out a max peers count based on the server limits
	maxPeers := srvr.MaxPeers
	if s.config.LightServ > 0 {
		maxPeers -= s.config.LightPeers
		if maxPeers < srvr.MaxPeers/2 {
			maxPeers = srvr.MaxPeers / 2
		}
	}
	// Start the networking layer and the light server if requested
	s.protocolManager.Start(maxPeers)
	if s.lesServer != nil {
		s.lesServer.Start(srvr)
	}
	return nil
}

// Stop implements node.Service, terminating all internal goroutines used by the
// Zerium protocol.
func (s *Zerium) Stop() error {
	if s.stopDbUpgrade != nil {
		s.stopDbUpgrade()
	}
	s.bloomIndexer.Close()
	s.blockchain.Stop()
	s.protocolManager.Stop()
	if s.lesServer != nil {
		s.lesServer.Stop()
	}
	s.txPool.Stop()
	s.miner.Stop()
	s.eventMux.Stop()

	s.chainDb.Close()
	close(s.shutdownChan)

	return nil
}