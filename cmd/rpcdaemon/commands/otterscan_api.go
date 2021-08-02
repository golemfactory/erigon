package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/RoaringBitmap/roaring/roaring64"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/hexutil"
	"github.com/ledgerwatch/erigon/consensus/ethash"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/ethdb"
	"github.com/ledgerwatch/erigon/ethdb/kv"
	"github.com/ledgerwatch/erigon/log"
	otterscan "github.com/ledgerwatch/erigon/otterscan/transactions"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/adapter/ethapi"
	"github.com/ledgerwatch/erigon/turbo/shards"
	"github.com/ledgerwatch/erigon/turbo/transactions"
	"math/big"
	"sync"
)

// API_LEVEL Must be incremented every time new additions are made
const API_LEVEL = 2

type SearchResult struct {
	BlockNumber uint64
}

type BlockSearchResult struct {
	hash common.Hash
}

type TransactionsWithReceipts struct {
	Txs       []*RPCTransaction        `json:"txs"`
	Receipts  []map[string]interface{} `json:"receipts"`
	FirstPage bool                     `json:"firstPage"`
	LastPage  bool                     `json:"lastPage"`
}

type OtterscanAPI interface {
	GetApiLevel() uint8
	GetInternalOperations(ctx context.Context, hash common.Hash) ([]*otterscan.InternalOperation, error)
	SearchTransactionsBefore(ctx context.Context, addr common.Address, blockNum uint64, minPageSize uint16) (*TransactionsWithReceipts, error)
	SearchTransactionsAfter(ctx context.Context, addr common.Address, blockNum uint64, minPageSize uint16) (*TransactionsWithReceipts, error)
	GetBlockDetails(ctx context.Context, number rpc.BlockNumber) (map[string]interface{}, error)
}

type OtterscanAPIImpl struct {
	*BaseAPI
	db kv.RoDB
}

func NewOtterscanAPI(base *BaseAPI, db kv.RoDB) *OtterscanAPIImpl {
	return &OtterscanAPIImpl{
		BaseAPI: base,
		db:      db,
	}
}

func (api *OtterscanAPIImpl) GetApiLevel() uint8 {
	return API_LEVEL
}

func (api *OtterscanAPIImpl) GetInternalOperations(ctx context.Context, hash common.Hash) ([]*otterscan.InternalOperation, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	txn, blockHash, _, txIndex, err := rawdb.ReadTransaction(tx, hash)
	if err != nil {
		return nil, err
	}
	if txn == nil {
		return nil, fmt.Errorf("transaction %#x not found", hash)
	}
	block, err := rawdb.ReadBlockByHash(tx, blockHash)
	if err != nil {
		return nil, err
	}

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}

	getHeader := func(hash common.Hash, number uint64) *types.Header {
		return rawdb.ReadHeader(tx, hash, number)
	}
	checkTEVM := ethdb.GetCheckTEVM(tx)
	msg, blockCtx, txCtx, ibs, _, err := transactions.ComputeTxEnv(ctx, block, chainConfig, getHeader, checkTEVM, ethash.NewFaker(), tx, blockHash, txIndex)
	if err != nil {
		return nil, err
	}

	tracer := otterscan.NewOperationsTracer(ctx)
	vmenv := vm.NewEVM(blockCtx, txCtx, ibs, chainConfig, vm.Config{Debug: true, Tracer: tracer})

	if _, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(msg.Gas()), true, false /* gasBailout */); err != nil {
		return nil, fmt.Errorf("tracing failed: %v", err)
	}

	return tracer.Results, nil
}

func (api *OtterscanAPIImpl) SearchTransactionsBefore(ctx context.Context, addr common.Address, blockNum uint64, minPageSize uint16) (*TransactionsWithReceipts, error) {
	dbtx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer dbtx.Rollback()

	fromCursor, err := dbtx.Cursor(kv.CallFromIndex)
	if err != nil {
		return nil, err
	}
	defer fromCursor.Close()
	toCursor, err := dbtx.Cursor(kv.CallToIndex)
	if err != nil {
		return nil, err
	}
	defer toCursor.Close()

	chainConfig, err := api.chainConfig(dbtx)
	if err != nil {
		return nil, err
	}

	// Initialize search cursors at the first shard >= desired block number
	resultCount := uint16(0)
	fromIter := newSearchBackIterator(fromCursor, addr, blockNum)
	toIter := newSearchBackIterator(toCursor, addr, blockNum)

	txs := make([]*RPCTransaction, 0)
	receipts := make([]map[string]interface{}, 0)

	multiIter, err := newMultiIterator(false, fromIter, toIter)
	if err != nil {
		return nil, err
	}
	eof := false
	for {
		if resultCount >= minPageSize || eof {
			break
		}

		var wg sync.WaitGroup
		results := make([]*TransactionsWithReceipts, 100, 100)
		tot := 0
		for i := 0; i < int(minPageSize-resultCount); i++ {
			var blockNum uint64
			blockNum, eof, err = multiIter()
			if err != nil {
				return nil, err
			}
			if eof {
				break
			}

			wg.Add(1)
			tot++
			go api.traceOneBlock(ctx, &wg, addr, chainConfig, i, blockNum, results)
		}
		wg.Wait()

		for i := 0; i < tot; i++ {
			r := results[i]
			if r == nil {
				return nil, errors.New("XXXX")
			}

			resultCount += uint16(len(r.Txs))
			for i := len(r.Txs) - 1; i >= 0; i-- {
				txs = append(txs, r.Txs[i])
			}
			for i := len(r.Receipts) - 1; i >= 0; i-- {
				receipts = append(receipts, r.Receipts[i])
			}

			if resultCount >= minPageSize {
				break
			}
		}
	}

	return &TransactionsWithReceipts{txs, receipts, blockNum == 0, eof}, nil
}

func newSearchBackIterator(cursor kv.Cursor, addr common.Address, maxBlock uint64) func() (uint64, bool, error) {
	search := make([]byte, common.AddressLength+8)
	copy(search[:common.AddressLength], addr.Bytes())
	if maxBlock == 0 {
		binary.BigEndian.PutUint64(search[common.AddressLength:], ^uint64(0))
	} else {
		binary.BigEndian.PutUint64(search[common.AddressLength:], maxBlock)
	}

	first := true
	var iter roaring64.IntIterable64

	return func() (uint64, bool, error) {
		if first {
			first = false
			k, v, err := cursor.Seek(search)
			if err != nil {
				return 0, true, err
			}
			if !bytes.Equal(k[:common.AddressLength], addr.Bytes()) {
				return 0, true, nil
			}

			bitmap := roaring64.New()
			if _, err := bitmap.ReadFrom(bytes.NewReader(v)); err != nil {
				return 0, true, err
			}
			iter = bitmap.ReverseIterator()
		}

		var blockNum uint64
		for {
			if !iter.HasNext() {
				// Try and check previous shard
				k, v, err := cursor.Prev()
				if err != nil {
					return 0, true, err
				}
				if !bytes.Equal(k[:common.AddressLength], addr.Bytes()) {
					return 0, true, nil
				}

				bitmap := roaring64.New()
				if _, err := bitmap.ReadFrom(bytes.NewReader(v)); err != nil {
					return 0, true, err
				}
				iter = bitmap.ReverseIterator()
			}
			blockNum = iter.Next()

			if maxBlock == 0 || blockNum < maxBlock {
				break
			}
		}
		return blockNum, false, nil
	}
}

func (api *OtterscanAPIImpl) SearchTransactionsAfter(ctx context.Context, addr common.Address, blockNum uint64, minPageSize uint16) (*TransactionsWithReceipts, error) {
	dbtx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer dbtx.Rollback()

	fromCursor, err := dbtx.Cursor(kv.CallFromIndex)
	if err != nil {
		return nil, err
	}
	defer fromCursor.Close()
	toCursor, err := dbtx.Cursor(kv.CallToIndex)
	if err != nil {
		return nil, err
	}
	defer toCursor.Close()

	chainConfig, err := api.chainConfig(dbtx)
	if err != nil {
		return nil, err
	}

	// Initialize search cursors at the first shard >= desired block number
	resultCount := uint16(0)
	fromIter := newSearchForwardIterator(fromCursor, addr, blockNum)
	toIter := newSearchForwardIterator(toCursor, addr, blockNum)

	txs := make([]*RPCTransaction, 0)
	receipts := make([]map[string]interface{}, 0)

	multiIter, err := newMultiIterator(true, fromIter, toIter)
	if err != nil {
		return nil, err
	}
	eof := false
	for {
		if resultCount >= minPageSize || eof {
			break
		}

		var wg sync.WaitGroup
		results := make([]*TransactionsWithReceipts, 100, 100)
		tot := 0
		for i := 0; i < int(minPageSize-resultCount); i++ {
			var blockNum uint64
			blockNum, eof, err = multiIter()
			if err != nil {
				return nil, err
			}
			if eof {
				break
			}

			wg.Add(1)
			tot++
			go api.traceOneBlock(ctx, &wg, addr, chainConfig, i, blockNum, results)
		}
		wg.Wait()

		for i := 0; i < tot; i++ {
			r := results[i]
			if r == nil {
				return nil, errors.New("XXXX")
			}

			resultCount += uint16(len(r.Txs))
			for _, v := range r.Txs {
				txs = append([]*RPCTransaction{v}, txs...)
			}
			for _, v := range r.Receipts {
				receipts = append([]map[string]interface{}{v}, receipts...)
			}

			if resultCount > minPageSize {
				break
			}
		}
	}

	return &TransactionsWithReceipts{txs, receipts, eof, blockNum == 0}, nil
}

func newSearchForwardIterator(cursor kv.Cursor, addr common.Address, minBlock uint64) func() (uint64, bool, error) {
	search := make([]byte, common.AddressLength+8)
	copy(search[:common.AddressLength], addr.Bytes())
	if minBlock == 0 {
		binary.BigEndian.PutUint64(search[common.AddressLength:], uint64(0))
	} else {
		binary.BigEndian.PutUint64(search[common.AddressLength:], minBlock)
	}

	first := true
	var iter roaring64.IntIterable64

	return func() (uint64, bool, error) {
		if first {
			first = false
			k, v, err := cursor.Seek(search)
			if err != nil {
				return 0, true, err
			}
			if !bytes.Equal(k[:common.AddressLength], addr.Bytes()) {
				return 0, true, nil
			}

			bitmap := roaring64.New()
			if _, err := bitmap.ReadFrom(bytes.NewReader(v)); err != nil {
				return 0, true, err
			}
			iter = bitmap.Iterator()
		}

		var blockNum uint64
		for {
			if !iter.HasNext() {
				// Try and check next shard
				k, v, err := cursor.Next()
				if err != nil {
					return 0, true, err
				}
				if !bytes.Equal(k[:common.AddressLength], addr.Bytes()) {
					return 0, true, nil
				}

				bitmap := roaring64.New()
				if _, err := bitmap.ReadFrom(bytes.NewReader(v)); err != nil {
					return 0, true, err
				}
				iter = bitmap.Iterator()
			}
			blockNum = iter.Next()

			if minBlock == 0 || blockNum > minBlock {
				break
			}
		}
		return blockNum, false, nil
	}
}

func newMultiIterator(smaller bool, fromIter func() (uint64, bool, error), toIter func() (uint64, bool, error)) (func() (uint64, bool, error), error) {
	nextFrom, fromEnd, err := fromIter()
	if err != nil {
		return nil, err
	}
	nextTo, toEnd, err := toIter()
	if err != nil {
		return nil, err
	}

	return func() (uint64, bool, error) {
		if fromEnd && toEnd {
			return 0, true, nil
		}

		var blockNum uint64
		if !fromEnd {
			blockNum = nextFrom
		}
		if !toEnd {
			if smaller {
				if nextTo < blockNum {
					blockNum = nextTo
				}
			} else {
				if nextTo > blockNum {
					blockNum = nextTo
				}
			}
		}

		// Pull next; it may be that from AND to contains the same blockNum
		if !fromEnd && blockNum == nextFrom {
			nextFrom, fromEnd, err = fromIter()
			if err != nil {
				return 0, false, err
			}
		}
		if !toEnd && blockNum == nextTo {
			nextTo, toEnd, err = toIter()
			if err != nil {
				return 0, false, err
			}
		}
		return blockNum, false, nil
	}, nil
}

func (api *OtterscanAPIImpl) traceOneBlock(ctx context.Context, wg *sync.WaitGroup, addr common.Address, chainConfig *params.ChainConfig, idx int, bNum uint64, results []*TransactionsWithReceipts) {
	defer wg.Done()

	// Trace block for Txs
	newdbtx, err := api.db.BeginRo(ctx)
	if err != nil {
		log.Error("ERR", "err", err)
		// TODO: signal error
		results[idx] = nil
	}
	defer newdbtx.Rollback()

	_, result, err := api.traceBlock(newdbtx, ctx, bNum, addr, chainConfig)
	if err != nil {
		// TODO: signal error
		log.Error("ERR", "err", err)
		results[idx] = nil
		//return nil, err
	}
	results[idx] = result
}

func (api *OtterscanAPIImpl) traceBlock(dbtx kv.Tx, ctx context.Context, blockNum uint64, searchAddr common.Address, chainConfig *params.ChainConfig) (bool, *TransactionsWithReceipts, error) {
	rpcTxs := make([]*RPCTransaction, 0)
	receipts := make([]map[string]interface{}, 0)

	// Retrieve the transaction and assemble its EVM context
	blockHash, err := rawdb.ReadCanonicalHash(dbtx, blockNum)
	if err != nil {
		return false, nil, err
	}

	block, senders, err := rawdb.ReadBlockWithSenders(dbtx, blockHash, blockNum)
	if err != nil {
		return false, nil, err
	}

	reader := state.NewPlainState(dbtx, blockNum-1)
	stateCache := shards.NewStateCache(32, 0 /* no limit */)
	cachedReader := state.NewCachedReader(reader, stateCache)
	noop := state.NewNoopWriter()
	cachedWriter := state.NewCachedWriter(noop, stateCache)

	ibs := state.New(cachedReader)
	signer := types.MakeSigner(chainConfig, blockNum)

	getHeader := func(hash common.Hash, number uint64) *types.Header {
		return rawdb.ReadHeader(dbtx, hash, number)
	}
	engine := ethash.NewFaker()
	checkTEVM := ethdb.GetCheckTEVM(dbtx)

	blockReceipts := rawdb.ReadReceipts(dbtx, block, senders)
	header := block.Header()
	found := false
	for idx, tx := range block.Transactions() {
		ibs.Prepare(tx.Hash(), block.Hash(), idx)

		msg, _ := tx.AsMessage(*signer, header.BaseFee)

		tracer := otterscan.NewTouchTracer(searchAddr)
		BlockContext := core.NewEVMBlockContext(header, getHeader, engine, nil, checkTEVM)
		TxContext := core.NewEVMTxContext(msg)

		vmenv := vm.NewEVM(BlockContext, TxContext, ibs, chainConfig, vm.Config{Debug: true, Tracer: tracer})
		if _, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(tx.GetGas()), true /* refunds */, false /* gasBailout */); err != nil {
			return false, nil, err
		}
		_ = ibs.FinalizeTx(vmenv.ChainConfig().Rules(block.NumberU64()), cachedWriter)

		if tracer.Found {
			rpcTx := newRPCTransaction(tx, block.Hash(), blockNum, uint64(idx), block.BaseFee())
			mReceipt := marshalReceipt(blockReceipts[idx], tx, chainConfig, block)
			mReceipt["timestamp"] = block.Time()
			rpcTxs = append(rpcTxs, rpcTx)
			receipts = append(receipts, mReceipt)
			found = true
		}
	}

	return found, &TransactionsWithReceipts{rpcTxs, receipts, false, false}, nil
}

func (api *OtterscanAPIImpl) delegateGetBlockByNumber(tx kv.Tx, b *types.Block, number rpc.BlockNumber) (map[string]interface{}, error) {
	additionalFields := make(map[string]interface{})

	td, err := rawdb.ReadTd(tx, b.Hash(), b.NumberU64())
	if err != nil {
		return nil, err
	}
	additionalFields["totalDifficulty"] = (*hexutil.Big)(td)
	additionalFields["transactionCount"] = b.Transactions().Len()
	response, err := ethapi.RPCMarshalBlock(b, false, false, additionalFields)

	if err == nil && number == rpc.PendingBlockNumber {
		// Pending blocks need to nil out a few fields
		for _, field := range []string{"hash", "nonce", "miner"} {
			response[field] = nil
		}
	}
	return response, err
}

func (api *OtterscanAPIImpl) delegateIssuance(tx kv.Tx, block *types.Block, chainConfig *params.ChainConfig) (Issuance, error) {
	if chainConfig.Ethash == nil {
		// Clique for example has no issuance
		return Issuance{}, nil
	}

	minerReward, uncleRewards := ethash.AccumulateRewards(chainConfig, block.Header(), block.Uncles())
	issuance := minerReward
	for _, r := range uncleRewards {
		p := r // avoids warning?
		issuance.Add(&issuance, &p)
	}

	var ret Issuance
	ret.BlockReward = hexutil.EncodeBig(minerReward.ToBig())
	ret.Issuance = hexutil.EncodeBig(issuance.ToBig())
	issuance.Sub(&issuance, &minerReward)
	ret.UncleReward = hexutil.EncodeBig(issuance.ToBig())
	return ret, nil
}

func (api *OtterscanAPIImpl) delegateBlockFees(ctx context.Context, tx kv.Tx, block *types.Block, chainConfig *params.ChainConfig) (uint64, error) {
	receipts, err := getReceipts(ctx, tx, chainConfig, block, nil)
	if err != nil {
		return 0, fmt.Errorf("getReceipts error: %v", err)
	}

	fees := uint64(0)
	for _, receipt := range receipts {
		txn := block.Transactions()[receipt.TransactionIndex]
		effectiveGasPrice := uint64(0)
		if !chainConfig.IsLondon(block.NumberU64()) {
			effectiveGasPrice = txn.GetPrice().Uint64()
		} else {
			baseFee, _ := uint256.FromBig(block.BaseFee())
			gasPrice := new(big.Int).Add(block.BaseFee(), txn.GetEffectiveGasTip(baseFee).ToBig())
			effectiveGasPrice = gasPrice.Uint64()
		}
		fees += effectiveGasPrice * receipt.GasUsed
	}

	return fees, nil
}

func (api *OtterscanAPIImpl) GetBlockDetails(ctx context.Context, number rpc.BlockNumber) (map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b, err := api.getBlockByNumber(number, tx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}

	chainConfig, err := api.chainConfig(tx)
	if err != nil {
		return nil, err
	}

	getBlockRes, err := api.delegateGetBlockByNumber(tx, b, number)
	if err != nil {
		return nil, err
	}
	getIssuanceRes, err := api.delegateIssuance(tx, b, chainConfig)
	if err != nil {
		return nil, err
	}
	feesRes, err := api.delegateBlockFees(ctx, tx, b, chainConfig)

	response := map[string]interface{}{}
	response["block"] = getBlockRes
	response["issuance"] = getIssuanceRes
	response["totalFees"] = hexutil.Uint64(feesRes)
	return response, nil
}
