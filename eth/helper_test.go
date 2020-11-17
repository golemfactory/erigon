// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// This file contains some shares testing functionality, common to  multiple
// different files and modules being tested.

package eth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"testing"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/u256"
	"github.com/ledgerwatch/turbo-geth/consensus/ethash"
	"github.com/ledgerwatch/turbo-geth/consensus/process"
	"github.com/ledgerwatch/turbo-geth/core"
	"github.com/ledgerwatch/turbo-geth/core/forkid"
	"github.com/ledgerwatch/turbo-geth/core/types"
	"github.com/ledgerwatch/turbo-geth/core/vm"
	"github.com/ledgerwatch/turbo-geth/crypto"
	"github.com/ledgerwatch/turbo-geth/eth/downloader"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/event"
	"github.com/ledgerwatch/turbo-geth/p2p"
	"github.com/ledgerwatch/turbo-geth/p2p/enode"
	"github.com/ledgerwatch/turbo-geth/params"
)

var (
	testBankKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	testBank       = crypto.PubkeyToAddress(testBankKey.PublicKey)
)

// newTestProtocolManager creates a new protocol manager for testing purposes,
// with the given number of blocks already known, and potential notification
// channels for different events.
func newTestProtocolManager(mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database, error) {
	dbGen := ethdb.NewMemDatabase() // This database is only used to generate the chain, then discarded
	defer dbGen.Close()
	var (
		evmux  = new(event.TypeMux)
		engine = ethash.NewFaker()
		gspec  = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc:  core.GenesisAlloc{testBank: {Balance: big.NewInt(1000000)}},
		}
		genesis = gspec.MustCommit(dbGen)
	)
	var chain []*types.Block
	// Fresh database
	db := ethdb.NewMemDatabase()

	eng := process.NewRemoteEngine(engine, params.TestChainConfig)
	defer eng.Close()

	// Regenerate genesis block in the fresh database
	gspec.MustCommit(db)
	blockchain, err := core.NewBlockChain(db, nil, gspec.Config, eng, vm.Config{}, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	blockchain.EnableReceipts(true)

	chain, _, err = core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), dbGen, blocks, generator, false /* intermediateHashes */)
	if err != nil {
		return nil, nil, fmt.Errorf("generate chain: %w", err)
	}

	if _, err = blockchain.InsertChain(context.Background(), chain); err != nil {
		return nil, nil, err
	}
	cht := &params.TrustedCheckpoint{}
	pm, err := NewProtocolManager(gspec.Config, cht, mode, DefaultConfig.NetworkID, evmux, &testTxPool{added: newtx, pool: make(map[common.Hash]*types.Transaction)}, eng, blockchain, db, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if err = pm.Start(1000, true); err != nil {
		return nil, nil, fmt.Errorf("error on protocol manager start: %w", err)
	}
	return pm, db, nil
}

// newTestProtocolManagerMust creates a new protocol manager for testing purposes,
// with the given number of blocks already known, and potential notification
// channels for different events. In case of an error, the constructor force-
// fails the test.
func newTestProtocolManagerMust(t *testing.T, mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, func()) {
	pm, db, err := newTestProtocolManager(mode, blocks, generator, newtx)
	if err != nil {
		t.Fatalf("Failed to create protocol manager: %v", err)
	}
	clear := func() {
		pm.Stop()
		pm.blockchain.Stop()
		db.Close()
	}
	return pm, clear
}

// testTxPool is a fake, helper transaction pool for testing purposes
type testTxPool struct {
	txFeed event.Feed
	pool   map[common.Hash]*types.Transaction // Hash map of collected transactions
	added  chan<- []*types.Transaction        // Notification channel for new transactions

	lock sync.RWMutex // Protects the transaction pool
}

// Has returns an indicator whether txpool has a transaction
// cached with the given hash.
func (p *testTxPool) Has(hash common.Hash) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash] != nil
}

// Get retrieves the transaction from local txpool with given
// tx hash.
func (p *testTxPool) Get(hash common.Hash) *types.Transaction {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash]
}

// AddRemotes appends a batch of transactions to the pool, and notifies any
// listeners if the addition channel is non nil
func (p *testTxPool) AddRemotes(txs []*types.Transaction) []error {
	p.lock.Lock()
	defer p.lock.Unlock()

	for _, tx := range txs {
		p.pool[tx.Hash()] = tx
	}
	if p.added != nil {
		p.added <- txs
	}
	p.txFeed.Send(core.NewTxsEvent{Txs: txs})
	return make([]error, len(txs))
}

// Pending returns all the transactions known to the pool
func (p *testTxPool) Pending() (map[common.Address]types.Transactions, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	batches := make(map[common.Address]types.Transactions)
	for _, tx := range p.pool {
		from, _ := types.Sender(types.HomesteadSigner{}, tx)
		batches[from] = append(batches[from], tx)
	}
	for _, batch := range batches {
		sort.Sort(types.TxByNonce(batch))
	}
	return batches, nil
}

func (p *testTxPool) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return p.txFeed.Subscribe(ch)
}

func (p *testTxPool) IsStarted() bool {
	return true
}

func (p *testTxPool) RunInit() error {
	return nil
}

func (p *testTxPool) RunStop() error {
	return nil
}

// newTestTransaction create a new dummy transaction.
func newTestTransaction(from *ecdsa.PrivateKey, nonce uint64, datasize int) *types.Transaction {
	tx := types.NewTransaction(nonce, common.Address{}, u256.Num0, 100000, u256.Num0, make([]byte, datasize))
	tx, _ = types.SignTx(tx, types.HomesteadSigner{}, from)
	return tx
}

// testPeer is a simulated peer to allow testing direct network calls.
type testPeer struct {
	net p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
	*peer
}

type testFirehosePeer struct {
	net  p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app  *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
	peer *firehosePeer
}

// newTestPeer creates a new peer registered at the given protocol manager.
func newTestPeer(name string, version int, pm *ProtocolManager, shake bool) (*testPeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Start the peer on a new thread
	var id enode.ID
	rand.Read(id[:])
	peer := pm.newPeer(version, p2p.NewPeer(id, name, nil), net, pm.txpool.Get)
	errc := make(chan error, 1)
	go func() { errc <- pm.runPeer(peer) }()
	tp := &testPeer{app: app, net: net, peer: peer}

	// Execute any implicitly requested handshakes and return
	if shake {
		var (
			genesis = pm.blockchain.Genesis()
			head    = pm.blockchain.CurrentHeader()
			td      = pm.blockchain.GetTd(head.Hash(), head.Number.Uint64())
		)
		forkID := forkid.NewID(pm.blockchain.Config(), pm.blockchain.Genesis().Hash(), pm.blockchain.CurrentHeader().Number.Uint64())
		tp.handshake(nil, td, head.Hash(), genesis.Hash(), forkID, forkid.NewFilter(pm.blockchain.Config(), genesis.Hash(), head.Number.Uint64()))

		// Newly connected peer will query the header that was announced during the handshake
		if err := p2p.ExpectMsg(tp.app, 0x03, &GetBlockHeadersData{Origin: HashOrNumber{Hash: pm.blockchain.CurrentBlock().Hash()}, Amount: 1}); err != nil {
			fmt.Printf("ExpectMsg error: %v\n", err)
			panic(err)
		}
		if err := p2p.Send(tp.app, 0x04, []*types.Header{pm.blockchain.CurrentBlock().Header()}); err != nil {
			panic(err)
		}
	}
	return tp, errc
}

func newFirehoseTestPeer(name string, pm *ProtocolManager) (*testFirehosePeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Generate a random id and create the peer
	var id enode.ID
	// #nosec G404
	if _, err := rand.Read(id[:]); err != nil {
		log.Fatal(err)
	}

	peer := &firehosePeer{Peer: p2p.NewPeer(id, name, nil), rw: net}

	// Start the peer on a new thread
	errc := make(chan error, 1)
	go func() {
		select {
		case <-pm.quitSync:
			errc <- p2p.DiscQuitting
		default:
			//errc <- pm.handleFirehose(peer)
		}
	}()

	tp := &testFirehosePeer{app: app, net: net, peer: peer}
	return tp, errc
}

// handshake simulates a trivial handshake that expects the same state from the
// remote side as we are simulating locally.
func (p *testPeer) handshake(t *testing.T, td *big.Int, head common.Hash, genesis common.Hash, forkID forkid.ID, forkFilter forkid.Filter) {
	var msg interface{}
	switch {
	case p.version >= eth64:
		msg = &StatusData{
			ProtocolVersion: uint32(p.version),
			NetworkID:       DefaultConfig.NetworkID,
			TD:              td,
			Head:            head,
			Genesis:         genesis,
			ForkID:          forkID,
		}
	default:
		panic(fmt.Sprintf("unsupported eth protocol version: %d", p.version))
	}
	if err := p2p.ExpectMsg(p.app, StatusMsg, msg); err != nil {
		t.Fatalf("status recv: %v", err)
	}
	if err := p2p.Send(p.app, StatusMsg, msg); err != nil {
		t.Fatalf("status send: %v", err)
	}
}

// close terminates the local side of the peer, notifying the remote protocol
// manager of termination.
func (p *testPeer) close() {
	p.app.Close()
}

func (p *testFirehosePeer) close() {
	p.app.Close()
}
